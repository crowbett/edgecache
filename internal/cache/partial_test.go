package cache

import (
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crowbett/edgecache/internal/cachekey"
)

const (
	digits  = "0123456789"
	lastMod = "Sat, 19 Sep 2026 11:00:00 GMT"
)

// rangeHarness stores digits under target with a strong ETag and Last-Modified.
func rangeHarness(t *testing.T, extra map[string]string, mut func(*Config)) *harness {
	t.Helper()
	h := newHarness(t, mut)
	hdr := map[string]string{"Cache-Control": "max-age=60", "ETag": `"v1"`, "Last-Modified": lastMod, "Content-Type": "text/plain"}
	for k, v := range extra {
		hdr[k] = v
	}
	h.o.set(static(200, hdr, digits))
	expect(t, h.do(http.MethodGet, target), 200, "MISS", digits)
	return h
}

type part struct{ contentRange, body string }

func readMultipart(t *testing.T, w *httptest.ResponseRecorder) []part {
	t.Helper()
	mt, params, err := mime.ParseMediaType(w.Header().Get("Content-Type"))
	if err != nil || mt != "multipart/byteranges" {
		t.Fatalf("Content-Type = %q (%v)", w.Header().Get("Content-Type"), err)
	}
	if cl := w.Header().Get("Content-Length"); cl != strconv.Itoa(w.Body.Len()) {
		t.Fatalf("Content-Length %s, body %d bytes", cl, w.Body.Len())
	}
	mr := multipart.NewReader(strings.NewReader(w.Body.String()), params["boundary"])
	var out []part
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("multipart: %v", err)
		}
		if ct := p.Header.Get("Content-Type"); ct != "text/plain" {
			t.Errorf("part Content-Type = %q", ct)
		}
		b, _ := io.ReadAll(p)
		out = append(out, part{p.Header.Get("Content-Range"), string(b)})
	}
}

func TestRangeFromStoredEntry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		hdr          []string
		extra        map[string]string // stored response headers
		plain        bool              // store readers without ReadAt
		wantStatus   int
		wantBody     string
		wantRange    string
		wantParts    []part
		wantNoLength bool
	}{
		{name: "single", hdr: []string{"Range", "bytes=2-5"}, wantStatus: 206, wantBody: "2345", wantRange: "bytes 2-5/10"},
		{name: "suffix", hdr: []string{"Range", "bytes=-3"}, wantStatus: 206, wantBody: "789", wantRange: "bytes 7-9/10"},
		{name: "open ended clamps", hdr: []string{"Range", "bytes=8-999"}, wantStatus: 206, wantBody: "89", wantRange: "bytes 8-9/10"},
		{name: "single without ReadAt", hdr: []string{"Range", "bytes=3-4"}, plain: true, wantStatus: 206, wantBody: "34", wantRange: "bytes 3-4/10"},
		{name: "one satisfiable of two is single-part", hdr: []string{"Range", "bytes=1-1,50-60"}, wantStatus: 206, wantBody: "1", wantRange: "bytes 1-1/10"},
		{
			name: "multipart", hdr: []string{"Range", "bytes=0-1, 7-"}, wantStatus: 206,
			wantParts: []part{{"bytes 0-1/10", "01"}, {"bytes 7-9/10", "789"}},
		},
		{
			name: "multipart out of order without ReadAt", hdr: []string{"Range", "bytes=6-6,-2,0-0"}, plain: true, wantStatus: 206,
			wantParts: []part{{"bytes 6-6/10", "6"}, {"bytes 8-9/10", "89"}, {"bytes 0-0/10", "0"}},
		},
		{name: "unsatisfiable", hdr: []string{"Range", "bytes=10-"}, wantStatus: 416, wantRange: "bytes */10"},
		{name: "malformed ignored", hdr: []string{"Range", "bytes=x-1"}, wantStatus: 200, wantBody: digits},
		{name: "other unit ignored", hdr: []string{"Range", "items=0-1"}, wantStatus: 200, wantBody: digits},
		{name: "overlap past size ignored", hdr: []string{"Range", "bytes=0-8,1-9"}, wantStatus: 200, wantBody: digits},
		{name: "If-Range etag match", hdr: []string{"Range", "bytes=0-0", "If-Range", `"v1"`}, wantStatus: 206, wantBody: "0", wantRange: "bytes 0-0/10"},
		{name: "If-Range etag mismatch", hdr: []string{"Range", "bytes=0-0", "If-Range", `"v0"`}, wantStatus: 200, wantBody: digits},
		{name: "If-Range weak etag", hdr: []string{"Range", "bytes=0-0", "If-Range", `W/"v1"`}, wantStatus: 200, wantBody: digits},
		{name: "If-Range date match", hdr: []string{"Range", "bytes=9-", "If-Range", lastMod}, wantStatus: 206, wantBody: "9", wantRange: "bytes 9-9/10"},
		{name: "If-Range date mismatch", hdr: []string{"Range", "bytes=9-", "If-Range", "Sat, 19 Sep 2026 10:00:00 GMT"}, wantStatus: 200, wantBody: digits},
		{name: "encoded single range", hdr: []string{"Range", "bytes=0-1"}, extra: map[string]string{"Content-Encoding": "gzip"}, wantStatus: 206, wantBody: "01", wantRange: "bytes 0-1/10"},
		{name: "encoded multi range falls back to 200", hdr: []string{"Range", "bytes=0-1,4-5"}, extra: map[string]string{"Content-Encoding": "gzip"}, wantStatus: 200, wantBody: digits},
		{name: "If-None-Match wins over Range", hdr: []string{"Range", "bytes=0-1", "If-None-Match", `"v1"`}, wantStatus: 304, wantNoLength: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := rangeHarness(t, tc.extra, nil)
			h.st.plainBodies = tc.plain
			w := h.do(http.MethodGet, target, tc.hdr...)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, tc.wantStatus, w.Body.String())
			}
			if got := w.Header().Get(HeaderXCache); got != "HIT" {
				t.Errorf("X-Cache = %q, want HIT", got)
			}
			if got := w.Header().Get("Content-Range"); got != tc.wantRange && tc.wantParts == nil {
				t.Errorf("Content-Range = %q, want %q", got, tc.wantRange)
			}
			switch {
			case tc.wantParts != nil:
				got := readMultipart(t, w)
				if len(got) != len(tc.wantParts) {
					t.Fatalf("parts = %+v, want %+v", got, tc.wantParts)
				}
				for i := range got {
					if got[i] != tc.wantParts[i] {
						t.Errorf("part %d = %+v, want %+v", i, got[i], tc.wantParts[i])
					}
				}
			case tc.wantNoLength:
				if w.Body.Len() != 0 {
					t.Errorf("body = %q", w.Body.String())
				}
			default:
				if w.Body.String() != tc.wantBody {
					t.Errorf("body = %q, want %q", w.Body.String(), tc.wantBody)
				}
				if cl := w.Header().Get("Content-Length"); cl != strconv.Itoa(len(tc.wantBody)) {
					t.Errorf("Content-Length = %q, want %d", cl, len(tc.wantBody))
				}
			}
			if w.Code == 206 && w.Header().Get("ETag") != `"v1"` {
				t.Errorf("206 lost the ETag (RFC 9110 §15.3.7): %v", w.Header())
			}
			if n := h.o.calls.Load(); n != 1 {
				t.Errorf("origin calls = %d, want 1 (ranges come from the store)", n)
			}
		})
	}
}

func TestHeadIgnoresRange(t *testing.T) {
	t.Parallel()
	h := rangeHarness(t, nil, nil)
	w := h.do(http.MethodHead, target, "Range", "bytes=0-1")
	expect(t, w, 200, "HIT", "")
	if w.Body.Len() != 0 || w.Header().Get("Content-Length") != "10" || w.Header().Get("Content-Range") != "" {
		t.Errorf("HEAD with Range: body %d, Content-Length %q, Content-Range %q", w.Body.Len(), w.Header().Get("Content-Length"), w.Header().Get("Content-Range"))
	}
	// A HEAD miss with Range goes upstream as a plain HEAD miss, not a range bypass.
	w = h.do(http.MethodHead, "http://example.com/other", "Range", "bytes=0-1")
	if w.Header().Get(HeaderXCache) != "MISS" || w.Header().Get(HeaderXCacheReason) != "" {
		t.Errorf("HEAD miss with Range: X-Cache %q reason %q", w.Header().Get(HeaderXCache), w.Header().Get(HeaderXCacheReason))
	}
}

// rangeOrigin answers Range requests with a 206 and plain GETs with the full
// body, both under hdr.
func rangeOrigin(hdr map[string]string) func(r *http.Request) (reply, error) {
	return func(r *http.Request) (reply, error) {
		if r.Header.Get("Range") == "bytes=0-3" {
			ph := map[string]string{"Content-Range": "bytes 0-3/10"}
			for k, v := range hdr {
				ph[k] = v
			}
			return rep(http.StatusPartialContent, ph, "0123"), nil
		}
		return rep(http.StatusOK, hdr, digits), nil
	}
}

func TestRangeMissFill(t *testing.T) {
	t.Parallel()
	cacheable := map[string]string{"Cache-Control": "max-age=60", "ETag": `"v1"`}
	tests := []struct {
		name        string
		fill        bool
		hdr         map[string]string
		maxObject   int64
		wantCalls   int32
		wantStored  bool
		wantSecond  string // X-Cache of the second range request
		wantStatus2 int
	}{
		{name: "fill stores the full body", fill: true, hdr: cacheable, wantCalls: 2, wantStored: true, wantSecond: "HIT", wantStatus2: 206},
		{name: "fill off", fill: false, hdr: cacheable, wantCalls: 2, wantSecond: "BYPASS", wantStatus2: 206},
		{name: "no-store is not filled", fill: true, hdr: map[string]string{"Cache-Control": "no-store"}, wantCalls: 2, wantSecond: "BYPASS", wantStatus2: 206},
		{name: "private is not filled", fill: true, hdr: map[string]string{"Cache-Control": "private, max-age=60"}, wantCalls: 2, wantSecond: "BYPASS", wantStatus2: 206},
		{name: "vary star is not filled", fill: true, hdr: map[string]string{"Cache-Control": "max-age=60", "Vary": "*"}, wantCalls: 2, wantSecond: "BYPASS", wantStatus2: 206},
		{name: "complete length over the object cap", fill: true, hdr: cacheable, maxObject: 5, wantCalls: 2, wantSecond: "BYPASS", wantStatus2: 206},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, func(c *Config) { c.RangeFill, c.MaxObjectBytes = tc.fill, tc.maxObject })
			h.o.set(rangeOrigin(tc.hdr))
			w := h.do(http.MethodGet, target, "Range", "bytes=0-3")
			expect(t, w, 206, "BYPASS", "0123")
			if w.Header().Get(HeaderXCacheReason) != "range" {
				t.Errorf("reason = %q", w.Header().Get(HeaderXCacheReason))
			}
			h.drain()
			if got := h.st.len() == 1; got != tc.wantStored {
				t.Fatalf("stored = %v, want %v", got, tc.wantStored)
			}
			w = h.do(http.MethodGet, target, "Range", "bytes=0-3")
			expect(t, w, tc.wantStatus2, tc.wantSecond, "0123")
			if n := h.o.calls.Load(); n != tc.wantCalls {
				t.Errorf("origin calls = %d, want %d", n, tc.wantCalls)
			}
			if tc.wantStored {
				h.o.mu.Lock()
				fill := h.o.reqs[1]
				h.o.mu.Unlock()
				if fill.Method != http.MethodGet || fill.Header.Get("Range") != "" {
					t.Errorf("fill request = %s Range=%q, want a plain GET", fill.Method, fill.Header.Get("Range"))
				}
				expect(t, h.do(http.MethodGet, target), 200, "HIT", digits)
			}
		})
	}
}

func TestRangeOnStaleEntryRefreshesInBackground(t *testing.T) {
	t.Parallel()
	h := rangeHarness(t, map[string]string{"Cache-Control": "max-age=1"}, nil)
	h.clk.Advance(2 * time.Second)
	h.o.set(func(r *http.Request) (reply, error) {
		if r.Header.Get("Range") != "" {
			return rep(206, map[string]string{"Content-Range": "bytes 0-0/10", "Cache-Control": "max-age=60"}, "0"), nil
		}
		return rep(304, map[string]string{"Cache-Control": "max-age=60", "ETag": `"v1"`}, ""), nil
	})
	w := h.do(http.MethodGet, target, "Range", "bytes=0-0")
	expect(t, w, 206, "BYPASS", "0")
	h.drain()
	if n := h.o.calls.Load(); n != 3 {
		t.Fatalf("origin calls = %d, want 3 (initial GET, range pass-through, background revalidation)", n)
	}
	w = h.do(http.MethodGet, target, "Range", "bytes=0-0")
	expect(t, w, 206, "HIT", "0")
}

func TestHeadMissDoesNotPoison(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.o.set(func(r *http.Request) (reply, error) {
		if r.Method == http.MethodHead {
			return rep(200, map[string]string{"Cache-Control": "max-age=60", "Content-Length": "5"}, ""), nil
		}
		return rep(200, map[string]string{"Cache-Control": "max-age=60"}, "hello"), nil
	})
	expect(t, h.do(http.MethodHead, target), 200, "MISS", "")
	if h.st.len() != 0 {
		t.Fatalf("HEAD miss stored %d entries", h.st.len())
	}
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "hello")
	w := h.do(http.MethodHead, target)
	expect(t, w, 200, "HIT", "")
	if w.Header().Get("Content-Length") != "5" {
		t.Errorf("HEAD hit Content-Length = %q", w.Header().Get("Content-Length"))
	}
	expect(t, h.do(http.MethodGet, target), 200, "HIT", "hello")
}

func TestHeadFreshensStoredGet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		head       reply
		wantStored bool
		wantFresh  bool // the next GET is a HIT without asking the origin
		wantMeta   string
	}{
		{
			name:       "validators match: updated and fresh again",
			head:       rep(200, map[string]string{"Cache-Control": "max-age=60", "ETag": `"v1"`, "Content-Length": "5", "X-Meta": "head"}, ""),
			wantStored: true, wantFresh: true, wantMeta: "head",
		},
		{
			name:       "etag differs: marked stale",
			head:       rep(200, map[string]string{"Cache-Control": "max-age=60", "ETag": `"v2"`}, ""),
			wantStored: true, wantMeta: "get",
		},
		{
			name:       "length differs: marked stale",
			head:       rep(200, map[string]string{"Cache-Control": "max-age=60", "ETag": `"v1"`, "Content-Length": "6"}, ""),
			wantStored: true, wantMeta: "get",
		},
		{
			name:       "no validator: marked stale",
			head:       rep(200, map[string]string{"Cache-Control": "max-age=60"}, ""),
			wantStored: true, wantMeta: "get",
		},
		{
			name: "no-store: dropped",
			head: rep(200, map[string]string{"Cache-Control": "no-store", "ETag": `"v1"`}, ""),
		},
		{
			name:       "non-200 leaves the entry alone",
			head:       rep(404, map[string]string{"Cache-Control": "max-age=60", "ETag": `"v1"`}, ""),
			wantStored: true, wantMeta: "get",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil)
			h.o.set(static(200, map[string]string{"Cache-Control": "max-age=1", "ETag": `"v1"`, "X-Meta": "get"}, "hello"))
			expect(t, h.do(http.MethodGet, target), 200, "MISS", "hello")
			h.clk.Advance(2 * time.Second)
			h.o.set(func(r *http.Request) (reply, error) {
				if r.Method != http.MethodHead {
					return rep(304, map[string]string{"Cache-Control": "max-age=60", "ETag": `"v1"`}, ""), nil
				}
				return tc.head, nil
			})
			w := h.do(http.MethodHead, target)
			if w.Code != tc.head.status || w.Body.Len() != 0 {
				t.Fatalf("HEAD = %d with %d body bytes", w.Code, w.Body.Len())
			}
			k := cachekey.Primary(http.MethodGet, mustURL(t, target))
			if got := h.st.bodyLen(k) == 5; got != tc.wantStored {
				t.Fatalf("stored = %v, want %v", got, tc.wantStored)
			}
			if !tc.wantStored {
				return
			}
			h.st.mu.Lock()
			meta := h.st.entries[k].meta
			h.st.mu.Unlock()
			if got := meta.Header.Get("X-Meta"); got != tc.wantMeta {
				t.Errorf("stored X-Meta = %q, want %q", got, tc.wantMeta)
			}
			before := h.o.calls.Load()
			w = h.do(http.MethodGet, target)
			if tc.wantFresh {
				expect(t, w, 200, "HIT", "hello")
				if h.o.calls.Load() != before {
					t.Error("fresh entry asked the origin")
				}
				if w.Header().Get("Content-Length") != "5" {
					t.Errorf("Content-Length = %q", w.Header().Get("Content-Length"))
				}
				return
			}
			expect(t, w, 200, "REVALIDATED", "hello")
			if meta.Lifetime > time.Second {
				t.Errorf("stale entry lifetime = %s", meta.Lifetime)
			}
		})
	}
}

func TestBackgroundRefreshIgnoresOrphanBudget(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.OrphanMaxBytes = 4 })
	var n int
	h.o.set(func(*http.Request) (reply, error) {
		n++
		return rep(200, map[string]string{"Cache-Control": "max-age=1, stale-while-revalidate=30"}, strings.Repeat(strconv.Itoa(n), 10)), nil
	})
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "1111111111")
	h.clk.Advance(2 * time.Second)
	expect(t, h.do(http.MethodGet, target), 200, "STALE", "1111111111")
	h.drain()
	expect(t, h.do(http.MethodGet, target), 200, "HIT", "2222222222")
}

// FuzzServeRange drives serveRange with arbitrary Range / If-Range values
// against a stored 26-byte body and checks every answer against the body: a
// 200 is the whole body, a single-part 206 is exactly the bytes its
// Content-Range names, a multipart 206 parses with the declared boundary and
// Content-Length and each part matches its own Content-Range, a 416 names the
// complete length and carries no body.
func FuzzServeRange(f *testing.F) {
	for _, s := range []string{
		"bytes=0-3", "bytes=-3", "bytes=4-", "bytes=0-1,5-6", "bytes=0-0,-1,10-12", "bytes=30-", "bytes=x",
		"bytes=0-25,0-0", "bytes=,,1-1", "BYTES=2-2", "bytes=-0", "bytes=25-25,0-0",
	} {
		f.Add(s, "")
		f.Add(s, `"v1"`)
	}
	f.Add("bytes=0-1", lastMod)
	f.Add("bytes=0-1", `W/"v1"`)
	const body = "abcdefghijklmnopqrstuvwxyz"
	f.Fuzz(func(t *testing.T, rng, ifRange string) {
		h := newHarness(t, nil)
		h.o.set(static(200, map[string]string{"Cache-Control": "max-age=60", "ETag": `"v1"`, "Last-Modified": lastMod, "Content-Type": "text/plain"}, body))
		h.do(http.MethodGet, target)
		r := httptest.NewRequest(http.MethodGet, target, http.NoBody)
		r.Header["Range"] = []string{rng}
		if ifRange != "" {
			r.Header["If-Range"] = []string{ifRange}
		}
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, r)
		slice := func(cr string) string {
			var a, b, n int
			if _, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &a, &b, &n); err != nil || n != len(body) || a < 0 || b < a || b >= n {
				t.Fatalf("Content-Range %q", cr)
			}
			return body[a : b+1]
		}
		switch w.Code {
		case http.StatusOK:
			if w.Body.String() != body {
				t.Fatalf("200 body %q", w.Body.String())
			}
		case http.StatusRequestedRangeNotSatisfiable:
			if w.Header().Get("Content-Range") != "bytes */26" || w.Body.Len() != 0 {
				t.Fatalf("416 Content-Range %q body %q", w.Header().Get("Content-Range"), w.Body.String())
			}
		case http.StatusPartialContent:
			if strings.HasPrefix(w.Header().Get("Content-Type"), "multipart/byteranges") {
				mt, params, err := mime.ParseMediaType(w.Header().Get("Content-Type"))
				if err != nil || mt != "multipart/byteranges" {
					t.Fatalf("Content-Type %q", w.Header().Get("Content-Type"))
				}
				if w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) {
					t.Fatalf("Content-Length %s for %d bytes", w.Header().Get("Content-Length"), w.Body.Len())
				}
				mr := multipart.NewReader(strings.NewReader(w.Body.String()), params["boundary"])
				parts := 0
				for {
					p, err := mr.NextPart()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatalf("multipart: %v", err)
					}
					got, _ := io.ReadAll(p)
					if want := slice(p.Header.Get("Content-Range")); string(got) != want {
						t.Fatalf("part %q, want %q", got, want)
					}
					parts++
				}
				if parts < 2 {
					t.Fatalf("multipart with %d parts", parts)
				}
				return
			}
			if want := slice(w.Header().Get("Content-Range")); w.Body.String() != want {
				t.Fatalf("206 body %q, want %q", w.Body.String(), want)
			}
			if w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) {
				t.Fatalf("Content-Length %s for %d bytes", w.Header().Get("Content-Length"), w.Body.Len())
			}
		default:
			t.Fatalf("status %d", w.Code)
		}
	})
}
