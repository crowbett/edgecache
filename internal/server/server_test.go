package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crowbett/edgecache/internal/cache"
	apihttp "github.com/crowbett/edgecache/internal/http"
	"github.com/crowbett/edgecache/internal/store"
)

// fakePurger records every request and answers with a fixed result or error.
type fakePurger struct {
	mu   sync.Mutex
	got  []cache.PurgeRequest
	res  cache.PurgeResult
	err  error
	fail bool
}

func (f *fakePurger) Purge(_ context.Context, req cache.PurgeRequest) (cache.PurgeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, req)
	if f.err != nil {
		return cache.PurgeResult{}, f.err
	}
	if f.fail {
		return cache.PurgeResult{}, errors.New("disk exploded")
	}
	return f.res, nil
}

func (f *fakePurger) last(t *testing.T) cache.PurgeRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) == 0 {
		t.Fatal("purger was not called")
	}
	return f.got[len(f.got)-1]
}

// fakeProxy is the catch-all: it flushes (proving no TimeoutHandler sits above)
// and answers with a marker status so tests can tell it from admin routes.
type fakeProxy struct {
	flushErr error
	calls    int
	mu       sync.Mutex
}

const proxyMarker = 299

func (p *fakeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	w.Header().Set("X-Cache", "HIT")
	w.WriteHeader(proxyMarker)
	_, _ = w.Write([]byte("chunk-1\n"))
	err := http.NewResponseController(w).Flush()
	p.mu.Lock()
	p.flushErr = err
	p.mu.Unlock()
	_, _ = w.Write([]byte("chunk-2 " + r.Method + " " + r.URL.Path + "\n"))
}

func testDeps(t *testing.T, purger *fakePurger) (Deps, *fakeProxy) {
	t.Helper()
	ready := &apihttp.Readiness{}
	ready.Set(true)
	proxy := &fakeProxy{}
	d := Deps{
		Proxy:  proxy,
		Purger: purger,
		Metrics: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			_, _ = w.Write([]byte("edgecache_requests_total{outcome=\"hit\"} 1\n"))
		}),
		Stats:   func() []byte { return []byte(`{"requests":{"hit":1}}`) },
		Ready:   ready,
		Log:     slog.New(slog.DiscardHandler),
		Version: "v9.9.9-test",
		StoreStats: func() map[string]store.Stats {
			return map[string]store.Stats{"mem": {Bytes: 10, Entries: 1, Capacity: 100}}
		},
		InFlight:    func() int { return 2 },
		TagIndexLen: func() int { return 7 },
		ConfigSummary: map[string]any{
			"mem_bytes": 256 << 20,
		},
	}
	return d, proxy
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json (body %q)", ct, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body is not JSON: %v: %q", err, rec.Body.String())
	}
	return m
}

func TestPurgeAuthMatrix(t *testing.T) {
	t.Parallel()
	const token = "s3cret"
	tests := []struct {
		name       string
		cfgToken   string
		header     string
		remote     string
		wantStatus int
		wantCalled bool
	}{
		{"token ok", token, token, "192.0.2.1:1234", http.StatusOK, true},
		{"token ok from loopback", token, token, "127.0.0.1:1234", http.StatusOK, true},
		{"token missing", token, "", "127.0.0.1:1234", http.StatusUnauthorized, false},
		{"token wrong", token, "nope", "127.0.0.1:1234", http.StatusForbidden, false},
		{"token wrong same length", token, "s3cre7", "127.0.0.1:1234", http.StatusForbidden, false},
		{"token prefix", token, "s3cre", "127.0.0.1:1234", http.StatusForbidden, false},
		{"token with suffix", token, token + "x", "127.0.0.1:1234", http.StatusForbidden, false},
		{"no token, loopback v4", "", "", "127.0.0.1:5555", http.StatusOK, true},
		{"no token, loopback v6", "", "", "[::1]:5555", http.StatusOK, true},
		{"no token, remote", "", "", "192.0.2.1:1234", http.StatusForbidden, false},
		{"no token, remote with header", "", "whatever", "192.0.2.1:1234", http.StatusForbidden, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			purger := &fakePurger{res: cache.PurgeResult{Purged: 3}}
			d, proxy := testDeps(t, purger)
			cfg := Config{ServiceName: "t", PurgeToken: tt.cfgToken}
			h := NewProxyHandler(cfg, d)

			for _, listener := range []string{"proxy", "admin"} {
				var req *http.Request
				var handler http.Handler
				if listener == "proxy" {
					req = httptest.NewRequest(MethodPurge, "http://example.com/obj/1?x=1", http.NoBody)
					handler = h
				} else {
					req = httptest.NewRequest(http.MethodPost, "/admin/purge", strings.NewReader(`{"url":"http://example.com/obj/1"}`))
					handler = NewAdminHandler(cfg, d)
				}
				req.RemoteAddr = tt.remote
				if tt.header != "" {
					req.Header.Set(PurgeHeader, tt.header)
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != tt.wantStatus {
					t.Errorf("%s: status = %d, want %d (body %q)", listener, rec.Code, tt.wantStatus, rec.Body.String())
				}
				body := decodeJSON(t, rec)
				if tt.wantStatus == http.StatusOK {
					if body["purged"] != float64(3) {
						t.Errorf("%s: purged = %v, want 3", listener, body["purged"])
					}
				} else if _, ok := body["error"]; !ok {
					t.Errorf("%s: error body missing: %v", listener, body)
				}
			}
			if proxy.calls != 0 {
				t.Errorf("PURGE reached the proxy handler %d times", proxy.calls)
			}
			purger.mu.Lock()
			called := len(purger.got) > 0
			purger.mu.Unlock()
			if called != tt.wantCalled {
				t.Errorf("purger called = %v, want %v", called, tt.wantCalled)
			}
			if tt.wantCalled {
				if got := purger.last(t); got.URL != "http://example.com/obj/1" || got.Prefix != "" || got.Tag != "" {
					t.Errorf("purge request = %+v", got)
				}
			}
		})
	}
}

func TestProxyPurgeURL(t *testing.T) {
	t.Parallel()
	purger := &fakePurger{}
	d, _ := testDeps(t, purger)
	h := NewProxyHandler(Config{PurgeToken: "t"}, d)
	req := httptest.NewRequest(MethodPurge, "/obj/42?b=2&a=1", http.NoBody)
	req.Host = "cache.example:8081"
	req.Header.Set(PurgeHeader, "t")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := purger.last(t).URL; got != "https://cache.example:8081/obj/42?b=2&a=1" {
		t.Errorf("url = %q", got)
	}
	if rec.Header().Get(apihttp.RequestIDHeader) == "" {
		t.Error("RequestID middleware missing on the proxy chain")
	}
}

func TestAdminPurgeJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		body       string
		purger     *fakePurger
		wantStatus int
		wantKind   string
		wantReq    cache.PurgeRequest
		wantKeys   int
	}{
		{"tag", `{"tag":"product-1"}`, &fakePurger{res: cache.PurgeResult{Purged: 12}}, http.StatusOK, "tag", cache.PurgeRequest{Tag: "product-1"}, 0},
		{"prefix", `{"prefix":"http://o/static/"}`, &fakePurger{res: cache.PurgeResult{Purged: 5}}, http.StatusOK, "prefix", cache.PurgeRequest{Prefix: "http://o/static/"}, 0},
		{"url verbose", `{"url":"http://o/a"}`, &fakePurger{res: cache.PurgeResult{Purged: 1, Keys: []string{"ab", "cd"}}}, http.StatusOK, "url", cache.PurgeRequest{URL: "http://o/a"}, 2},
		{"bad selector", `{}`, &fakePurger{err: cache.ErrBadPurge}, http.StatusBadRequest, "", cache.PurgeRequest{}, 0},
		{"two selectors", `{"url":"http://o/a","tag":"x"}`, &fakePurger{err: cache.ErrBadPurge}, http.StatusBadRequest, "", cache.PurgeRequest{}, 0},
		{"engine failure", `{"tag":"x"}`, &fakePurger{fail: true}, http.StatusInternalServerError, "", cache.PurgeRequest{}, 0},
		{"malformed json", `{"tag":`, &fakePurger{}, http.StatusBadRequest, "", cache.PurgeRequest{}, 0},
		{"unknown field", `{"tags":["x"]}`, &fakePurger{}, http.StatusBadRequest, "", cache.PurgeRequest{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _ := testDeps(t, tt.purger)
			h := NewAdminHandler(Config{ServiceName: "t", RequestTimeout: 2 * time.Second}, d)
			req := httptest.NewRequest(http.MethodPost, "/admin/purge", strings.NewReader(tt.body))
			req.RemoteAddr = "127.0.0.1:9"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			body := decodeJSON(t, rec)
			if tt.wantStatus != http.StatusOK {
				msg, _ := body["error"].(string)
				if msg == "" || strings.Contains(msg, "exploded") {
					t.Errorf("error body = %v (internals must not leak)", body)
				}
				return
			}
			if body["kind"] != tt.wantKind || body["purged"] != float64(tt.purger.res.Purged) {
				t.Errorf("body = %v", body)
			}
			keys, _ := body["keys"].([]any)
			if len(keys) != tt.wantKeys {
				t.Errorf("keys = %v, want %d", keys, tt.wantKeys)
			}
			if got := tt.purger.last(t); got != tt.wantReq {
				t.Errorf("purge request = %+v, want %+v", got, tt.wantReq)
			}
		})
	}

	t.Run("wrong method", func(t *testing.T) {
		t.Parallel()
		d, _ := testDeps(t, &fakePurger{})
		h := NewAdminHandler(Config{RequestTimeout: time.Second}, d)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/purge", http.NoBody))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", rec.Code)
		}
	})
}

func TestAdminStatsShape(t *testing.T) {
	t.Parallel()
	d, _ := testDeps(t, &fakePurger{})
	cfg := Config{ServiceName: "edgecache", Addr: "127.0.0.1:8081", AdminAddr: "127.0.0.1:9101", PurgeToken: "x", RequestTimeout: time.Second, ShutdownTimeout: 15 * time.Second}
	h := NewAdminHandler(cfg, d)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/stats", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var s Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.Service != "edgecache" || s.Version != "v9.9.9-test" || s.UptimeSeconds < 0 {
		t.Errorf("header fields: %+v", s)
	}
	if s.Store["mem"].Bytes != 10 || s.CollapseInFlight != 2 || s.TagIndexLen != 7 {
		t.Errorf("store/collapse/tagindex: %+v", s)
	}
	if s.Config["mem_bytes"] != float64(256<<20) || s.Config["purge_token_set"] != true || s.Config["addr"] != "127.0.0.1:8081" {
		t.Errorf("config summary: %v", s.Config)
	}
	if _, leaked := s.Config["purge_token"]; leaked {
		t.Error("purge token leaked into stats")
	}
	var m map[string]any
	if err := json.Unmarshal(s.Metrics, &m); err != nil || m["requests"] == nil {
		t.Errorf("metrics snapshot not embedded: %s", s.Metrics)
	}
	raw := decodeJSON(t, rec)
	for _, k := range []string{"service", "version", "uptime_seconds", "store", "collapse_inflight", "tagindex_len", "config", "metrics"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("stats missing key %q", k)
		}
	}

	// Optional callbacks absent: still a valid document.
	h = NewAdminHandler(cfg, Deps{Log: d.Log})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/stats", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("bare deps status = %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil || s.Version != "dev" {
		t.Errorf("bare deps: %v %+v", err, s)
	}
}

func TestAdminProbesAndMetrics(t *testing.T) {
	t.Parallel()
	d, _ := testDeps(t, &fakePurger{})
	h := NewAdminHandler(Config{RequestTimeout: time.Second}, d)
	for _, tt := range []struct {
		path       string
		ready      bool
		wantStatus int
		wantBody   string
	}{
		{"/healthz", true, http.StatusOK, `"ok"`},
		{"/readyz", true, http.StatusOK, `"ready"`},
		{"/readyz", false, http.StatusServiceUnavailable, `"not ready"`},
		{"/metrics", true, http.StatusOK, "edgecache_requests_total"},
		{"/nope", true, http.StatusNotFound, ""},
	} {
		d.Ready.Set(tt.ready)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, http.NoBody))
		if rec.Code != tt.wantStatus || !strings.Contains(rec.Body.String(), tt.wantBody) {
			t.Errorf("%s ready=%v: status %d body %q", tt.path, tt.ready, rec.Code, rec.Body.String())
		}
	}
}

func TestPprofOnlyOnAdmin(t *testing.T) {
	t.Parallel()
	d, proxy := testDeps(t, &fakePurger{})
	cfg := Config{RequestTimeout: 2 * time.Second}
	admin := NewAdminHandler(cfg, d)
	proxyH := NewProxyHandler(cfg, d)

	rec := httptest.NewRecorder()
	admin.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", http.NoBody))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "goroutine") {
		t.Errorf("admin pprof index: status %d body %.80q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	admin.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/cmdline", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Errorf("admin pprof cmdline: status %d", rec.Code)
	}

	// On the proxy listener /debug/pprof/ is just another origin path.
	rec = httptest.NewRecorder()
	proxyH.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", http.NoBody))
	if rec.Code != proxyMarker || proxy.calls != 1 {
		t.Errorf("proxy /debug/pprof/: status %d, proxy calls %d (want pass-through)", rec.Code, proxy.calls)
	}
}

// TestProxyHasNoTimeoutHandler proves the streaming contract: a Flush through
// the whole proxy chain succeeds (http.TimeoutHandler's writer cannot flush).
func TestProxyHasNoTimeoutHandler(t *testing.T) {
	t.Parallel()
	d, proxy := testDeps(t, &fakePurger{})
	h := NewProxyHandler(Config{ServiceName: "t", RequestTimeout: time.Second}, d)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/obj/1", http.NoBody))
	if rec.Code != proxyMarker {
		t.Fatalf("status = %d, want %d", rec.Code, proxyMarker)
	}
	if proxy.flushErr != nil {
		t.Fatalf("Flush through the proxy chain failed: %v (is a TimeoutHandler in the chain?)", proxy.flushErr)
	}
	if !rec.Flushed {
		t.Error("recorder was not flushed")
	}
	if rec.Header().Get("X-Cache") != "HIT" || !strings.Contains(rec.Body.String(), "chunk-2 GET /obj/1") {
		t.Errorf("response altered by the chain: %v %q", rec.Header(), rec.Body.String())
	}
	if rec.Header().Get(apihttp.RequestIDHeader) == "" {
		t.Error("RequestID header missing")
	}

	// Control: the same handler behind the template Timeout cannot flush.
	wrapped := apihttp.Chain(proxy, apihttp.Timeout(time.Second))
	rec = httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/obj/1", http.NoBody))
	if proxy.flushErr == nil {
		t.Error("control: Flush through http.TimeoutHandler unexpectedly succeeded")
	}
}

func TestProxyRecoversPanics(t *testing.T) {
	t.Parallel()
	d, _ := testDeps(t, &fakePurger{})
	d.Proxy = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	h := NewProxyHandler(Config{}, d)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", http.NoBody))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestNewServers(t *testing.T) {
	t.Parallel()
	d, _ := testDeps(t, &fakePurger{})
	cfg := Config{Addr: "127.0.0.1:0", AdminAddr: "127.0.0.1:1", RequestTimeout: 30 * time.Second}
	proxy, admin := NewServers(cfg, d)
	if proxy.Addr != cfg.Addr || admin.Addr != cfg.AdminAddr {
		t.Errorf("addrs: %s %s", proxy.Addr, admin.Addr)
	}
	if proxy.WriteTimeout != 0 {
		t.Errorf("proxy WriteTimeout = %s, want 0 (streaming)", proxy.WriteTimeout)
	}
	if proxy.ReadHeaderTimeout != 10*time.Second || proxy.IdleTimeout != 120*time.Second {
		t.Errorf("proxy timeouts: rh=%s idle=%s", proxy.ReadHeaderTimeout, proxy.IdleTimeout)
	}
	if admin.WriteTimeout != 35*time.Second || admin.ReadTimeout != 15*time.Second || admin.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("admin timeouts: w=%s r=%s rh=%s", admin.WriteTimeout, admin.ReadTimeout, admin.ReadHeaderTimeout)
	}
	if proxy.Handler == nil || admin.Handler == nil || proxy.ErrorLog == nil || admin.ErrorLog == nil {
		t.Error("handlers or error logs not set")
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	t.Parallel()
	for addr, want := range map[string]bool{
		"127.0.0.1:1": true, "[::1]:1": true, "127.5.5.5:80": true, "localhost:1": true,
		"192.0.2.1:1": false, "[2001:db8::1]:1": false, "": false, "garbage": false, "10.0.0.1": false,
	} {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestTokenEqual(t *testing.T) {
	t.Parallel()
	cases := []struct {
		got, want string
		eq        bool
	}{
		{"s3cret", "s3cret", true},
		{"", "", true},
		{"s3cret", "s3cre", false},
		{"s3cre", "s3cret", false},
		{"s3cret", "S3CRET", false},
		{"", "s3cret", false},
		{strings.Repeat("a", 4096), strings.Repeat("a", 4096), true},
		{strings.Repeat("a", 4096), strings.Repeat("a", 4095) + "b", false},
	}
	for _, tc := range cases {
		if got := tokenEqual(tc.got, tc.want); got != tc.eq {
			t.Errorf("tokenEqual(%q, %q) = %v, want %v", tc.got, tc.want, got, tc.eq)
		}
	}
}
