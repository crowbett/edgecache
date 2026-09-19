package bench

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/crowbett/edgecache/internal/cachekey"
	"github.com/crowbett/edgecache/internal/memstore"
	"github.com/crowbett/edgecache/internal/rfc9111"
	"github.com/crowbett/edgecache/internal/store"
)

// Fixed clock: nothing here calls time.Now() on the measured path.
var (
	benchNow  = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	benchURL  = mustParse("http://origin.example:8082/obj/123?b=2&a=1")
	benchVary = []string{"accept", "accept-encoding"}
	benchReq  = http.Header{
		"Accept":          {"text/html,application/xhtml+xml"},
		"Accept-Encoding": {"gzip, br"},
		"User-Agent":      {"oha/1.16"},
		"Cache-Control":   {"max-age=0"},
	}
	benchResp = http.Header{
		"Content-Type":   {"application/octet-stream"},
		"Content-Length": {"16384"},
		"Cache-Control":  {"public, max-age=60, stale-while-revalidate=30"},
		"Date":           {benchNow.Add(-10 * time.Second).Format(http.TimeFormat)},
		"Last-Modified":  {benchNow.Add(-24 * time.Hour).Format(http.TimeFormat)},
		"Etag":           {`"abc123"`},
		"Vary":           {"Accept-Encoding, Accept"},
		"Connection":     {"keep-alive"},
		"Keep-Alive":     {"timeout=5"},
		"Server":         {"slow-origin"},
	}
	benchPolicy = rfc9111.Policy{HeuristicFraction: 0.1, HeuristicMax: 24 * time.Hour}
	benchBody   = bytes.Repeat([]byte("edgecache-bench-"), 1024) // 16 KiB
	sinkKey     cachekey.Key
	sinkInt     int64
)

func mustParse(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// storedResp is the persisted view of benchResp with hop-by-hop stripped.
func storedResp() rfc9111.Resp {
	return rfc9111.Resp{
		Status:       http.StatusOK,
		Header:       rfc9111.StripHopByHop(benchResp),
		RequestTime:  benchNow.Add(-10 * time.Second),
		ResponseTime: benchNow.Add(-10*time.Second + 50*time.Millisecond),
		Lifetime:     60 * time.Second,
	}
}

// BenchmarkKeyBuild is primary + secondary over two Vary headers, the whole
// per-request key derivation. Target: 0 allocs/op (cachekey owns it: Key is a
// [32]byte, Secondary hashes through a pooled scratch buffer).
func BenchmarkKeyBuild(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		p := cachekey.Primary(http.MethodGet, benchURL)
		sinkKey = cachekey.Secondary(p, benchVary, benchReq)
	}
}

// BenchmarkKeyPrimary isolates Canonical + sha256 of the method/URL.
func BenchmarkKeyPrimary(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		sinkKey = cachekey.Primary(http.MethodGet, benchURL)
	}
}

// BenchmarkKeySecondary isolates the Vary-selected secondary key.
func BenchmarkKeySecondary(b *testing.B) {
	p := cachekey.Primary(http.MethodGet, benchURL)
	b.ReportAllocs()
	for b.Loop() {
		sinkKey = cachekey.Secondary(p, benchVary, benchReq)
	}
}

// BenchmarkParseResponse parses a typical origin Cache-Control (with an RFC
// 5861 extension). Target: < 300 ns, <= 1 alloc.
func BenchmarkParseResponse(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		sd := rfc9111.ParseResponse(benchResp)
		sinkInt += int64(sd.MaxAge)
	}
}

// BenchmarkParseRequest parses a client Cache-Control. Target: < 100 ns, 0 allocs.
func BenchmarkParseRequest(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		rd := rfc9111.ParseRequest(benchReq)
		sinkInt += int64(rd.MaxAge)
	}
}

// BenchmarkEvaluateFresh is the policy decision on a fresh stored entry with a
// fixed clock. Target: < 100 ns, 0 allocs.
func BenchmarkEvaluateFresh(b *testing.B) {
	res := storedResp()
	sd := rfc9111.ParseResponse(res.Header)
	rd := rfc9111.ParseRequest(http.Header{})
	b.ReportAllocs()
	for b.Loop() {
		d := benchPolicy.Evaluate(benchNow, res, sd, rd)
		sinkInt += int64(d.Age)
	}
}

// BenchmarkAge is the §4.2.3 age computation. Target: < 50 ns, 0 allocs.
func BenchmarkAge(b *testing.B) {
	res := storedResp()
	b.ReportAllocs()
	for b.Loop() {
		sinkInt += int64(rfc9111.Age(res, benchNow))
	}
}

// BenchmarkStripHopByHop copies an origin header without the hop-by-hop
// fields (once per miss). Target: < 1 us.
func BenchmarkStripHopByHop(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		h := rfc9111.StripHopByHop(benchResp)
		sinkInt += int64(len(h))
	}
}

// BenchmarkWriteHeaders is the hit-path header copy: stored header -> response
// header (values aliased, net/http never mutates them) plus Age and X-Cache.
// Target: < 500 ns, <= 2 allocs (the Age string and the X-Cache slice).
func BenchmarkWriteHeaders(b *testing.B) {
	stored := rfc9111.StripHopByHop(benchResp)
	dst := make(http.Header, 16)
	b.ReportAllocs()
	for b.Loop() {
		clear(dst)
		for k, v := range stored {
			dst[k] = v
		}
		dst["Age"] = []string{strconv.FormatInt(int64(10), 10)}
		dst["X-Cache"] = []string{"HIT"}
	}
	sinkInt += int64(len(dst))
}

// memFixture stores one 16 KiB body in a real memstore and returns it with its
// key, or skips when memstore's bodies have not landed yet (Writer errors).
func memFixture(b *testing.B) (*memstore.Store, cachekey.Key) {
	b.Helper()
	s, err := memstore.New(memstore.Config{Capacity: 64 << 20, Shards: 16, MaxObjectBytes: 1 << 20, MaxVariants: 8})
	if err != nil {
		b.Skipf("memstore not landed: %v", err)
	}
	k := cachekey.Primary(http.MethodGet, benchURL)
	meta := store.Meta{Resp: storedResp(), Method: http.MethodGet, URL: cachekey.Canonical(benchURL), Size: int64(len(benchBody))}
	w, err := s.Writer(context.Background(), k, meta)
	if err != nil {
		b.Skipf("memstore not landed: %v", err)
	}
	if _, err := w.Write(benchBody); err != nil {
		b.Fatal(err)
	}
	e, err := w.Commit(meta)
	if err != nil {
		b.Fatal(err)
	}
	e.Release()
	if _, err := s.Get(context.Background(), k); errors.Is(err, store.ErrMiss) {
		b.Skip("memstore not landed: committed entry is a miss")
	}
	return s, k
}

// BenchmarkMemGetHit is the memory-tier round trip: Get -> Open -> copy 16 KiB
// -> Close -> Release. Target: < 1 us, <= 4 allocs (entry, reader, closer).
func BenchmarkMemGetHit(b *testing.B) {
	s, k := memFixture(b)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	b.ReportAllocs()
	b.SetBytes(int64(len(benchBody)))
	for b.Loop() {
		e, err := s.Get(ctx, k)
		if err != nil {
			b.Fatal(err)
		}
		rc, err := e.Open()
		if err != nil {
			b.Fatal(err)
		}
		n, _ := io.Copy(io.Discard, rc)
		_ = rc.Close()
		e.Release()
		sinkInt += n
	}
}

// discardWriter is an http.ResponseWriter that keeps headers and drops the body,
// so BenchmarkHitPath measures the engine-side work, not the recorder.
type discardWriter struct {
	h      http.Header
	status int
	n      int64
}

func (w *discardWriter) Header() http.Header         { return w.h }
func (w *discardWriter) WriteHeader(code int)        { w.status = code }
func (w *discardWriter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }

// BenchmarkHitPath is what the engine does on a fresh hit, end to end: key
// build, Get, Evaluate with a fixed clock, header copy + Age + X-Cache, body
// copy to a ResponseWriter, Release. Target: < 2 us, <= 4 allocs.
func BenchmarkHitPath(b *testing.B) {
	s, _ := memFixture(b)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	rd := rfc9111.ParseRequest(benchReq)
	w := &discardWriter{h: make(http.Header, 16)}
	b.ReportAllocs()
	b.SetBytes(int64(len(benchBody)))
	for b.Loop() {
		k := cachekey.Primary(http.MethodGet, benchURL)
		e, err := s.Get(ctx, k)
		if err != nil {
			b.Fatal(err)
		}
		sd := rfc9111.ParseResponse(e.Meta.Header)
		d := benchPolicy.Evaluate(benchNow, e.Meta.Resp, sd, rd)
		clear(w.h)
		for hk, hv := range e.Meta.Header {
			w.h[hk] = hv
		}
		w.h["Age"] = []string{strconv.FormatInt(int64(d.Age/time.Second), 10)}
		w.h["X-Cache"] = []string{"HIT"}
		w.WriteHeader(e.Meta.Status)
		rc, err := e.Open()
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(w, rc)
		_ = rc.Close()
		e.Release()
	}
	sinkInt += w.n
}
