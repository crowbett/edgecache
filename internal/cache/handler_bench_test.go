package cache_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/crowbett/edgecache/internal/cache"
	"github.com/crowbett/edgecache/internal/memstore"
	"github.com/crowbett/edgecache/internal/metrics"
)

// benchOrigin answers every fetch with the same cacheable 16 KiB body.
type benchOrigin struct{ body []byte }

func (o benchOrigin) Do(context.Context, *http.Request) (*http.Response, error) {
	h := http.Header{
		"Cache-Control":  {"public, max-age=3600"},
		"Content-Type":   {"application/octet-stream"},
		"Content-Length": {strconv.Itoa(len(o.body))},
		"Etag":           {`"bench"`},
	}
	return &http.Response{StatusCode: http.StatusOK, Header: h, ContentLength: int64(len(o.body)), Body: io.NopCloser(bytes.NewReader(o.body))}, nil
}

// nopWriter keeps headers, drops the body.
type nopWriter struct {
	h http.Header
	n int64
}

func (w *nopWriter) Header() http.Header         { return w.h }
func (w *nopWriter) WriteHeader(int)             {}
func (w *nopWriter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }

// BenchmarkHandlerHit is a fresh hit through the real engine handler (not the
// synthetic BenchmarkHitPath): relative request URL + Host as net/http hands
// it over, memstore, the metrics registry as recorder, a fixed clock. The
// "xfp" case adds an X-Forwarded-Proto header from a peer that is not a
// trusted proxy.
func BenchmarkHandlerHit(b *testing.B) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		hdr  http.Header
	}{
		{"plain", http.Header{"Accept": {"*/*"}, "User-Agent": {"oha/1.16"}}},
		{"xfp", http.Header{"Accept": {"*/*"}, "User-Agent": {"oha/1.16"}, "X-Forwarded-Proto": {"https"}}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			st, err := memstore.New(memstore.Config{Capacity: 64 << 20, Shards: 16, MaxObjectBytes: 1 << 20, MaxVariants: 8})
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			c, err := cache.New(cache.Config{MaxVariants: 8, Now: func() time.Time { return now }},
				st, benchOrigin{body: bytes.Repeat([]byte("edgecache-bench-"), 1024)}, metrics.NewRegistry(nil), slog.New(slog.DiscardHandler))
			if err != nil {
				b.Fatal(err)
			}
			defer c.Stop()
			h := c.Handler()
			r := httptest.NewRequest(http.MethodGet, "/obj/123?b=2&a=1", http.NoBody)
			r.URL.Scheme, r.URL.Host = "", ""
			r.Host = "127.0.0.1:8081"
			r.Header = tc.hdr
			w := &nopWriter{h: make(http.Header, 16)}
			h.ServeHTTP(w, r) // miss: fills the entry
			b.ReportAllocs()
			b.SetBytes(16 << 10)
			for b.Loop() {
				clear(w.h)
				h.ServeHTTP(w, r)
			}
			if w.h.Get("X-Cache") != "HIT" {
				b.Fatalf("X-Cache = %q", w.h.Get("X-Cache"))
			}
		})
	}
}
