package cache

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// FuzzOriginHeaders drives the whole engine with arbitrary origin response
// headers (the boundary where bytes from another process become cache state):
// two requests for the same URL plus one HEAD and one conditional, asserting
// the handler never panics, every response carries a valid X-Cache label and
// a well-formed Age, a hit never outlives the store's copy of the headers, a
// stored entry's headers are hop-by-hop free, and the tag index never grows
// past the entries the store holds.
func FuzzOriginHeaders(f *testing.F) {
	imf := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC).Format(http.TimeFormat)
	f.Add("max-age=60", imf, "", "", `"e"`, "", "", 200)
	f.Add("no-store", imf, "", "", "", "", "", 200)
	f.Add("max-age=60", imf, "", "", "", "accept-encoding", "t1, t2", 200)
	f.Add("max-age=0, stale-while-revalidate=30", imf, "", imf, "", "*", "", 200)
	f.Add("", imf, "junk", imf, "W/\"w\"", "Accept, accept", "", 404)
	f.Add("private, max-age=10", imf, "", "", "", "", "", 200)
	f.Add("max-age=60", "junk", "-1", "junk", "", "", ",,,", 500)
	f.Add("s-maxage=1, must-revalidate", imf, "", "", `"e"`, "", "", 301)
	f.Add("max-age=60", imf, "", "", "", "", "", 304)
	f.Add("max-age=60", imf, "", "", "", "", "", 206)
	f.Add("max-age=60, no-cache=\"X-Secret\"", imf, "", "", "", "", "", 200)
	f.Add("max-age=99999999999", imf, "", "", "", "", "", 599)
	f.Add("max-age=60", imf, "", "", "", "", "", 100)
	f.Add("max-age=60", imf, "", "", "", "", "", 0)
	f.Fuzz(func(t *testing.T, cc, date, expires, lastMod, etag, vary, tags string, status int) {
		if status < 100 || status > 599 || status == http.StatusSwitchingProtocols {
			return // net/http itself rejects these before a handler sees them
		}
		h := newHarness(t, func(c *Config) { c.MaxVariants = 2 })
		h.c.log = slog.New(slog.DiscardHandler)
		hdr := map[string]string{
			"Cache-Control": cc, "Date": date, "Expires": expires, "Last-Modified": lastMod,
			"ETag": etag, "Vary": vary, "Cache-Tag": tags, "Connection": "X-Hop", "X-Hop": "1", "X-Secret": "s",
		}
		for k, v := range hdr {
			if v == "" {
				delete(hdr, k)
			}
		}
		h.o.set(static(status, hdr, "body"))

		labels := map[string]bool{"HIT": true, "MISS": true, "STALE": true, "REVALIDATED": true, "BYPASS": true}
		check := func(w *httptest.ResponseRecorder) {
			t.Helper()
			if x := w.Header().Get(HeaderXCache); x != "" && !labels[x] {
				t.Fatalf("X-Cache %q", x)
			}
			if age := w.Header().Get("Age"); age != "" && strings.TrimLeft(age, "0123456789") != "" {
				t.Fatalf("Age %q", age)
			}
			if w.Header().Get("X-Hop") != "" || w.Header().Get("Connection") != "" {
				t.Fatal("hop-by-hop field reached the client")
			}
			if w.Code < 100 || w.Code > 599 {
				t.Fatalf("status %d", w.Code)
			}
		}
		check(h.do(http.MethodGet, target, "Accept-Encoding", "gzip"))
		check(h.do(http.MethodGet, target, "Accept-Encoding", "gzip"))
		check(h.do(http.MethodHead, target))
		check(h.do(http.MethodGet, target, "If-None-Match", etag))
		check(h.do(http.MethodGet, target, "Accept-Encoding", "br", "Cache-Control", "max-stale"))
		h.clk.Advance(2 * time.Hour)
		check(h.do(http.MethodGet, target))
		h.drain()
		h.c.janitor(t.Context()) // reconciles the index with the store's own evictions (variant cap)

		if n, stored := h.c.TagIndexLen(), h.st.len(); n > stored {
			t.Fatalf("tag index has %d rows for %d entries", n, stored)
		}
		h.st.mu.Lock()
		defer h.st.mu.Unlock()
		for k, e := range h.st.entries {
			for _, name := range []string{"Connection", "X-Hop", "Keep-Alive", "Transfer-Encoding"} {
				if e.meta.Header.Get(name) != "" {
					t.Fatalf("stored entry %s carries hop-by-hop %s", k, name)
				}
			}
			if strings.Contains(cc, `no-cache="X-Secret"`) && e.meta.Header.Get("X-Secret") != "" {
				t.Fatalf("stored entry %s carries a no-cache=\"X-Secret\" field", k)
			}
			if e.meta.Status < 100 || e.meta.Status > 599 || e.meta.Status == http.StatusNotModified || e.meta.Status == http.StatusPartialContent {
				t.Fatalf("stored entry with status %d", e.meta.Status)
			}
			if e.meta.Size != int64(len(e.body)) {
				t.Fatalf("stored entry size %d for %d body bytes", e.meta.Size, len(e.body))
			}
		}
	})
}
