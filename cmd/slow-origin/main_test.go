package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crowbett/edgecache/internal/cache"
	"github.com/crowbett/edgecache/internal/memstore"
	"github.com/crowbett/edgecache/internal/metrics"
	"github.com/crowbett/edgecache/internal/origin"
)

// reply is a fully read response.
type reply struct {
	code   int
	header http.Header
	body   string
}

func do(t *testing.T, client *http.Client, method, u string, hdr ...string) reply {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, u, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return reply{code: resp.StatusCode, header: resp.Header, body: string(b)}
}

func get(t *testing.T, client *http.Client, u string, hdr ...string) reply {
	t.Helper()
	return do(t, client, http.MethodGet, u, hdr...)
}

func hits(t *testing.T, base string) hitsSnapshot {
	t.Helper()
	r := get(t, http.DefaultClient, base+"/admin/hits")
	if r.code != http.StatusOK {
		t.Fatalf("/admin/hits: %d", r.code)
	}
	var snap hitsSnapshot
	if err := json.Unmarshal([]byte(r.body), &snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestHandlerServesKeyspaceAndValidators(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(Handler(Config{Latency: 2 * time.Millisecond, Jitter: time.Millisecond, BodyBytes: 64, CacheControl: ModeETag, Keyspace: 3}))
	defer srv.Close()

	r := get(t, http.DefaultClient, srv.URL+"/obj/2")
	if r.code != http.StatusOK || len(r.body) != 64 || !strings.HasPrefix(r.body, "key=2\n") {
		t.Fatalf("obj/2: %d %q", r.code, r.body)
	}
	if r.header.Get("Cache-Control") != "max-age=1" || r.header.Get("ETag") == "" || r.header.Get("Last-Modified") == "" {
		t.Errorf("headers: %v", r.header)
	}
	if got := get(t, http.DefaultClient, srv.URL+"/obj/3"); got.code != http.StatusNotFound {
		t.Errorf("outside keyspace: %d", got.code)
	}
	if got := get(t, http.DefaultClient, srv.URL+"/obj/2", "If-None-Match", r.header.Get("ETag")); got.code != http.StatusNotModified {
		t.Errorf("If-None-Match: %d", got.code)
	}
	if got := get(t, http.DefaultClient, srv.URL+"/obj/2", "If-Modified-Since", r.header.Get("Last-Modified")); got.code != http.StatusNotModified {
		t.Errorf("If-Modified-Since: %d", got.code)
	}
	snap := hits(t, srv.URL)
	if snap.Total != 3 || snap.NotModified != 2 || snap.Keys["2"] != 3 {
		t.Errorf("hits = %+v", snap)
	}
	if got := do(t, http.DefaultClient, http.MethodDelete, srv.URL+"/admin/hits"); got.code != http.StatusNoContent {
		t.Fatalf("reset: %d", got.code)
	}
	if snap := hits(t, srv.URL); snap.Total != 0 {
		t.Errorf("after reset: %+v", snap)
	}
}

func TestHandlerModes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode, cc, vary string
	}{
		{"max-age=60", "max-age=60", ""},
		{"no-store", "no-store", ""},
		{ModeVary, "max-age=60", "Accept-Encoding"},
		{ModeSWR, "max-age=1, stale-while-revalidate=30, stale-if-error=60", ""},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(Handler(Config{BodyBytes: 8, CacheControl: tc.mode, Keyspace: 1}))
			defer srv.Close()
			r := get(t, http.DefaultClient, srv.URL+"/obj/0")
			if r.header.Get("Cache-Control") != tc.cc || r.header.Get("Vary") != tc.vary {
				t.Errorf("Cache-Control=%q Vary=%q", r.header.Get("Cache-Control"), r.header.Get("Vary"))
			}
		})
	}
}

func TestHandlerRatePacesBody(t *testing.T) {
	t.Parallel()
	const size, rate = 128 << 10, 1 << 20 // ~125 ms at 1 MiB/s
	srv := httptest.NewServer(Handler(Config{BodyBytes: size, Rate: rate, CacheControl: "max-age=60", Keyspace: 1}))
	defer srv.Close()
	start := time.Now()
	r := get(t, http.DefaultClient, srv.URL+"/obj/0")
	elapsed := time.Since(start)
	if r.code != http.StatusOK || len(r.body) != size {
		t.Fatalf("status %d, %d bytes; want 200, %d", r.code, len(r.body), size)
	}
	if !strings.HasPrefix(r.body, "key=0\n") {
		t.Errorf("body starts %q", r.body[:8])
	}
	if elapsed < 90*time.Millisecond {
		t.Errorf("paced body arrived in %s, want >= ~125ms", elapsed)
	}
}

// TestEndToEndThroughCache mounts the real engine (memstore + origin client +
// metrics) in front of the in-process origin: repeated GETs hit once upstream,
// an unsafe method invalidates, and the metrics exposition counts it all.
func TestEndToEndThroughCache(t *testing.T) {
	t.Parallel()
	up := httptest.NewServer(Handler(Config{Latency: 5 * time.Millisecond, Jitter: 2 * time.Millisecond, BodyBytes: 4096, CacheControl: "max-age=60", Keyspace: 20}))
	defer up.Close()

	base, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	o, err := origin.New(origin.Config{Base: base, HeaderTimeout: 5 * time.Second, Total: 10 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer o.CloseIdleConnections()
	st, err := memstore.New(memstore.Config{Capacity: 8 << 20, Shards: 4, MaxObjectBytes: 1 << 20, MaxVariants: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	reg := metrics.NewRegistry(map[string]string{"version": "test"})
	c, err := cache.New(cache.Config{
		MaxObjectBytes:      1 << 20,
		MaxVariants:         8,
		OrphanFetch:         true,
		OrphanMaxBytes:      1 << 20,
		CollapseLeadTimeout: 10 * time.Second,
	}, st, o, reg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(runDone)
	}()
	defer func() {
		cancel()
		<-runDone
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dcancel()
		if err := c.Drain(dctx); err != nil {
			t.Errorf("Drain: %v", err)
		}
	}()

	proxy := httptest.NewServer(c.Handler())
	defer proxy.Close()
	client := proxy.Client()

	for i, want := range []string{"MISS", "HIT", "HIT"} {
		r := get(t, client, proxy.URL+"/obj/7")
		if r.code != http.StatusOK || len(r.body) != 4096 || !strings.HasPrefix(r.body, "key=7\n") {
			t.Fatalf("request %d: %d, %d bytes", i, r.code, len(r.body))
		}
		if got := r.header.Get("X-Cache"); got != want {
			t.Fatalf("request %d: X-Cache = %q, want %q", i, got, want)
		}
		if i > 0 && r.header.Get("Age") == "" {
			t.Errorf("request %d: no Age header", i)
		}
	}
	if snap := hits(t, up.URL); snap.Total != 1 {
		t.Fatalf("origin hits = %d, want 1", snap.Total)
	}

	// HEAD from the stored GET.
	if r := do(t, client, http.MethodHead, proxy.URL+"/obj/7"); r.header.Get("X-Cache") != "HIT" || r.header.Get("Content-Length") != "4096" {
		t.Errorf("HEAD: X-Cache=%q Content-Length=%q", r.header.Get("X-Cache"), r.header.Get("Content-Length"))
	}

	// Purge by URL, then a POST invalidates again after the refill.
	res, err := c.Purge(context.Background(), cache.PurgeRequest{URL: proxy.URL + "/obj/7"})
	if err != nil || res.Purged != 1 {
		t.Fatalf("purge: %+v %v", res, err)
	}
	if r := get(t, client, proxy.URL+"/obj/7"); r.header.Get("X-Cache") != "MISS" {
		t.Errorf("after purge: X-Cache = %q", r.header.Get("X-Cache"))
	}
	if r := do(t, client, http.MethodPost, proxy.URL+"/obj/7"); r.code != http.StatusNoContent || r.header.Get("X-Cache") != "BYPASS" {
		t.Fatalf("POST: %d %q", r.code, r.header.Get("X-Cache"))
	}
	if r := get(t, client, proxy.URL+"/obj/7"); r.header.Get("X-Cache") != "MISS" {
		t.Errorf("after POST: X-Cache = %q", r.header.Get("X-Cache"))
	}
	if snap := hits(t, up.URL); snap.Total != 3 {
		t.Errorf("origin GET hits = %d, want 3", snap.Total)
	}

	rr := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", http.NoBody))
	text := rr.Body.String()
	for _, want := range []string{`outcome="hit"`, `outcome="miss"`, `outcome="bypass"`, "edgecache_origin_requests_total", "edgecache_purges_total"} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}
