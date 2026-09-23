package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crowbett/edgecache/internal/cachekey"
	"github.com/crowbett/edgecache/internal/store"
)

// ---- fakes -----------------------------------------------------------------

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fakeEntry struct {
	meta store.Meta
	body []byte
}

// fakeStore is an in-memory Store with a variant cap, refcount accounting and Walk.
type fakeStore struct {
	mu          sync.Mutex
	entries     map[cachekey.Key]*fakeEntry
	variants    map[cachekey.Key][]store.Variant
	held        atomic.Int32
	maxVariants int
	writerErr   error
	plainBodies bool // Open returns a reader without ReadAt/Seek
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		entries:     make(map[cachekey.Key]*fakeEntry),
		variants:    make(map[cachekey.Key][]store.Variant),
		maxVariants: 8,
	}
}

type readSeekCloser struct{ *bytes.Reader }

func (readSeekCloser) Close() error { return nil }

func (s *fakeStore) entry(k cachekey.Key, e *fakeEntry) *store.Entry {
	s.held.Add(1)
	body := e.body
	plain := s.plainBodies
	return store.NewEntry(k, e.meta, store.TierMem,
		func() (io.ReadCloser, error) {
			if plain {
				return io.NopCloser(struct{ io.Reader }{bytes.NewReader(body)}), nil
			}
			return readSeekCloser{bytes.NewReader(body)}, nil
		},
		func() { s.held.Add(-1) })
}

func (s *fakeStore) Get(_ context.Context, k cachekey.Key) (*store.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[k]
	if !ok {
		return nil, store.ErrMiss
	}
	return s.entry(k, e), nil
}

type fakeWriter struct {
	s   *fakeStore
	k   cachekey.Key
	buf bytes.Buffer
}

func (w *fakeWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }

func (w *fakeWriter) Commit(final store.Meta) (*store.Entry, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	e := &fakeEntry{meta: final, body: bytes.Clone(w.buf.Bytes())}
	w.s.entries[w.k] = e
	return w.s.entry(w.k, e), nil
}

func (w *fakeWriter) Abort() error { return nil }

func (s *fakeStore) Writer(_ context.Context, k cachekey.Key, _ store.Meta) (store.Writer, error) {
	if s.writerErr != nil {
		return nil, s.writerErr
	}
	return &fakeWriter{s: s, k: k}, nil
}

func (s *fakeStore) UpdateMeta(_ context.Context, k cachekey.Key, m store.Meta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[k]
	if !ok {
		return store.ErrMiss
	}
	e.meta = m
	return nil
}

func (s *fakeStore) Delete(_ context.Context, keys ...cachekey.Key) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, k := range keys {
		if _, ok := s.entries[k]; ok {
			delete(s.entries, k)
			n++
		}
	}
	return n, nil
}

func (s *fakeStore) Variants(_ context.Context, primary cachekey.Key) ([]store.Variant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.Variant(nil), s.variants[primary]...), nil
}

func (s *fakeStore) PutVariant(_ context.Context, primary cachekey.Key, v store.Variant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.variants[primary]
	out := make([]store.Variant, 0, len(list)+1)
	out = append(out, v)
	for _, old := range list {
		if old.Key != v.Key {
			out = append(out, old)
		}
	}
	for len(out) > s.maxVariants {
		victim := out[len(out)-1]
		out = out[:len(out)-1]
		delete(s.entries, victim.Key)
	}
	s.variants[primary] = out
	return nil
}

func (s *fakeStore) Stats() map[string]store.Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b int64
	for _, e := range s.entries {
		b += int64(len(e.body))
	}
	return map[string]store.Stats{store.TierMem: {Bytes: b, Entries: int64(len(s.entries))}}
}

func (s *fakeStore) Close() error { return nil }

func (s *fakeStore) Walk(_ context.Context, fn func(cachekey.Key, store.Meta) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.entries {
		if err := fn(k, e.meta); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeStore) Sweep(context.Context) (int, error) { return 0, nil }

func (s *fakeStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func (s *fakeStore) bodyLen(k cachekey.Key) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[k]; ok {
		return len(e.body)
	}
	return -1
}

// reply is what a fake origin answers; fakeOrigin.Do turns it into the
// *http.Response the engine consumes (and closes). stream, when set, is the
// body as is with an unknown length.
type reply struct {
	status int
	hdr    map[string]string
	body   string
	stream io.ReadCloser
}

// fakeOrigin is a Fetcher driven by a function; it counts calls and keeps the requests.
type fakeOrigin struct {
	mu    sync.Mutex
	calls atomic.Int32
	reqs  []*http.Request
	fn    func(r *http.Request) (reply, error)
}

func (o *fakeOrigin) Do(ctx context.Context, r *http.Request) (*http.Response, error) {
	o.calls.Add(1)
	o.mu.Lock()
	o.reqs = append(o.reqs, r.Clone(context.Background()))
	fn := o.fn
	o.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rp, err := fn(r)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	for k, v := range rp.hdr {
		h.Set(k, v)
	}
	resp := &http.Response{StatusCode: rp.status, Header: h}
	if rp.stream != nil {
		resp.Body, resp.ContentLength = rp.stream, -1
		return resp, nil
	}
	if rp.body != "" {
		h.Set("Content-Length", strconv.Itoa(len(rp.body)))
	}
	resp.Body, resp.ContentLength = io.NopCloser(strings.NewReader(rp.body)), int64(len(rp.body))
	return resp, nil
}

func (o *fakeOrigin) set(fn func(r *http.Request) (reply, error)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.fn = fn
}

func rep(status int, hdr map[string]string, body string) reply {
	return reply{status: status, hdr: hdr, body: body}
}

func static(status int, hdr map[string]string, body string) func(*http.Request) (reply, error) {
	return func(*http.Request) (reply, error) { return rep(status, hdr, body), nil }
}

// fakeRecorder collects every event.
type fakeRecorder struct {
	mu        sync.Mutex
	outcomes  []string
	collapsed int
	origin    int
	purged    map[string]int
	evicted   int
	gauges    int
}

func (r *fakeRecorder) Request(outcome string, _ int, _, _ time.Duration, _ int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes = append(r.outcomes, outcome)
}

func (r *fakeRecorder) Origin(int, time.Duration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.origin++
}

func (r *fakeRecorder) Collapsed(waiters int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.collapsed += waiters
}

func (r *fakeRecorder) StoreGauges(map[string]store.Stats) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges++
}

func (r *fakeRecorder) Purged(kind string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.purged == nil {
		r.purged = map[string]int{}
	}
	r.purged[kind] += n
}

func (r *fakeRecorder) VariantEvicted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evicted++
}

func (r *fakeRecorder) outcomeList() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.outcomes...)
}

// ---- harness ---------------------------------------------------------------

type harness struct {
	t   *testing.T
	c   *Cache
	st  *fakeStore
	o   *fakeOrigin
	rec *fakeRecorder
	clk *clock
	h   http.Handler
}

func newHarness(t *testing.T, mut func(*Config)) *harness {
	t.Helper()
	clk := &clock{t: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)}
	cfg := Config{
		MaxVariants:         8,
		OrphanFetch:         true,
		OrphanMaxBytes:      8 << 20,
		CollapseLeadTimeout: 5 * time.Second,
		Now:                 clk.Now,
		Debug:               true,
	}
	if mut != nil {
		mut(&cfg)
	}
	st := newFakeStore()
	st.maxVariants = cfg.MaxVariants
	o := &fakeOrigin{fn: static(http.StatusOK, map[string]string{"Cache-Control": "max-age=60"}, "hello")}
	rec := &fakeRecorder{}
	c, err := New(cfg, st, o, rec, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Drain(ctx)
		c.Stop()
		if held := st.held.Load(); held != 0 {
			t.Errorf("%d entries still held after the test", held)
		}
	})
	return &harness{t: t, c: c, st: st, o: o, rec: rec, clk: clk, h: c.Handler()}
}

func (h *harness) do(method, target string, hdr ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.doCtx(context.Background(), method, target, hdr...)
}

func (h *harness) doCtx(ctx context.Context, method, target string, hdr ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	r := httptest.NewRequestWithContext(ctx, method, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

func (h *harness) drain() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.c.Drain(ctx); err != nil {
		h.t.Fatalf("Drain: %v", err)
	}
}

func expect(t *testing.T, w *httptest.ResponseRecorder, status int, xcache, body string) {
	t.Helper()
	if w.Code != status {
		t.Errorf("status = %d, want %d (body %q)", w.Code, status, w.Body.String())
	}
	if got := w.Header().Get(HeaderXCache); got != xcache {
		t.Errorf("X-Cache = %q, want %q (reason %q)", got, xcache, w.Header().Get(HeaderXCacheReason))
	}
	if body != "" && w.Body.String() != body {
		t.Errorf("body = %q, want %q", w.Body.String(), body)
	}
}

const target = "http://example.com/obj/1"

// ---- tests -----------------------------------------------------------------

func TestMissThenHitWithAge(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "hello")
	if h.do(http.MethodGet, target).Header().Get("Age") != "0" {
		t.Error("fresh hit should carry Age: 0")
	}
	h.clk.Advance(10 * time.Second)
	w := h.do(http.MethodGet, target)
	expect(t, w, 200, "HIT", "hello")
	if w.Header().Get("Age") != "10" {
		t.Errorf("Age = %q, want 10", w.Header().Get("Age"))
	}
	if w.Header().Get("Content-Length") != "5" {
		t.Errorf("Content-Length = %q, want 5", w.Header().Get("Content-Length"))
	}
	if w.Header().Get(HeaderXCacheKey) == "" {
		t.Error("Debug should add X-Cache-Key")
	}
	if n := h.o.calls.Load(); n != 1 {
		t.Errorf("origin calls = %d, want 1", n)
	}
	if got := h.rec.outcomeList(); strings.Join(got, ",") != "miss,hit,hit" {
		t.Errorf("outcomes = %v", got)
	}
	if h.rec.origin != 1 {
		t.Errorf("Recorder.Origin calls = %d, want 1", h.rec.origin)
	}
}

func TestMissKeysOnHostWhenURLIsRelative(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	// A real server request has only the path in r.URL; Host carries the authority.
	r := httptest.NewRequest(http.MethodGet, "/obj/1", http.NoBody)
	r.URL.Scheme, r.URL.Host = "", ""
	r.Host = "example.com"
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	expect(t, w, 200, "MISS", "hello")
	expect(t, h.do(http.MethodGet, target), 200, "HIT", "hello")
	res, err := h.c.Purge(context.Background(), PurgeRequest{URL: target})
	if err != nil || res.Purged != 1 {
		t.Fatalf("Purge = %+v, %v; want 1 purged", res, err)
	}
}

func TestHeadServedFromStoredGet(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "hello")
	w := h.do(http.MethodHead, target)
	expect(t, w, 200, "HIT", "")
	if w.Body.Len() != 0 || w.Header().Get("Content-Length") != "5" {
		t.Errorf("HEAD hit: body %d bytes, Content-Length %q", w.Body.Len(), w.Header().Get("Content-Length"))
	}
	// A HEAD miss is forwarded as HEAD and stores nothing.
	h.o.set(func(r *http.Request) (reply, error) {
		if r.Method != http.MethodHead {
			return reply{}, fmt.Errorf("origin got %s, want HEAD", r.Method)
		}
		return rep(200, map[string]string{"Cache-Control": "max-age=60"}, ""), nil
	})
	expect(t, h.do(http.MethodHead, "http://example.com/other"), 200, "MISS", "")
	if h.st.len() != 1 {
		t.Errorf("store has %d entries, want 1", h.st.len())
	}
}

func TestVaryVariants(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.o.set(func(r *http.Request) (reply, error) {
		enc := r.Header.Get("Accept-Encoding")
		return rep(200, map[string]string{"Cache-Control": "max-age=60", "Vary": "Accept-Encoding"}, "enc="+enc), nil
	})
	expect(t, h.do(http.MethodGet, target, "Accept-Encoding", "gzip"), 200, "MISS", "enc=gzip")
	expect(t, h.do(http.MethodGet, target, "Accept-Encoding", "identity"), 200, "MISS", "enc=identity")
	expect(t, h.do(http.MethodGet, target, "Accept-Encoding", "gzip"), 200, "HIT", "enc=gzip")
	expect(t, h.do(http.MethodGet, target, "Accept-Encoding", "identity"), 200, "HIT", "enc=identity")
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "enc=")
	if n := h.o.calls.Load(); n != 3 {
		t.Errorf("origin calls = %d, want 3", n)
	}
	vs, _ := h.st.Variants(context.Background(), cachekey.Primary(http.MethodGet, mustURL(t, target)))
	if len(vs) != 3 {
		t.Errorf("variants = %d, want 3", len(vs))
	}

	h.o.set(static(200, map[string]string{"Cache-Control": "max-age=60", "Vary": "*"}, "star"))
	w := h.do(http.MethodGet, "http://example.com/star")
	expect(t, w, 200, "BYPASS", "star")
	if w.Header().Get(HeaderXCacheReason) != "vary-star" {
		t.Errorf("reason = %q", w.Header().Get(HeaderXCacheReason))
	}
}

func TestVariantCapCountsEviction(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.MaxVariants = 2 })
	h.o.set(func(r *http.Request) (reply, error) {
		return rep(200, map[string]string{"Cache-Control": "max-age=60", "Vary": "X-V"}, r.Header.Get("X-V")), nil
	})
	for _, v := range []string{"a", "b", "c"} {
		expect(t, h.do(http.MethodGet, target, "X-V", v), 200, "MISS", v)
	}
	if h.rec.evicted != 1 {
		t.Errorf("VariantEvicted = %d, want 1", h.rec.evicted)
	}
	expect(t, h.do(http.MethodGet, target, "X-V", "a"), 200, "MISS", "a") // evicted with its variant
}

func TestStaleWhileRevalidate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	var version atomic.Int32
	h.o.set(func(*http.Request) (reply, error) {
		v := version.Add(1)
		return rep(200, map[string]string{"Cache-Control": "max-age=1, stale-while-revalidate=30"}, "v"+strconv.Itoa(int(v))), nil
	})
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "v1")
	h.clk.Advance(2 * time.Second)
	w := h.do(http.MethodGet, target)
	expect(t, w, 200, "STALE", "v1")
	if w.Header().Get("Age") != "2" {
		t.Errorf("Age = %q, want 2", w.Header().Get("Age"))
	}
	h.drain()
	if n := h.o.calls.Load(); n != 2 {
		t.Fatalf("origin calls = %d, want 2 (one background refresh)", n)
	}
	w = h.do(http.MethodGet, target)
	expect(t, w, 200, "HIT", "v2")
	if w.Header().Get("Age") != "0" {
		t.Errorf("Age after refresh = %q, want 0", w.Header().Get("Age"))
	}
}

func TestRevalidate304Merges(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.o.set(static(200, map[string]string{"Cache-Control": "max-age=1", "ETag": `"v1"`, "X-Meta": "old"}, "hello"))
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "hello")
	h.clk.Advance(2 * time.Second)
	h.o.set(func(r *http.Request) (reply, error) {
		if r.Header.Get("If-None-Match") != `"v1"` {
			return reply{}, fmt.Errorf("no conditional: %v", r.Header)
		}
		return rep(304, map[string]string{"Cache-Control": "max-age=60", "ETag": `"v1"`, "X-Meta": "new"}, ""), nil
	})
	w := h.do(http.MethodGet, target)
	expect(t, w, 200, "REVALIDATED", "hello")
	if w.Header().Get("X-Meta") != "new" || w.Header().Get("Age") != "0" {
		t.Errorf("merged headers: X-Meta=%q Age=%q", w.Header().Get("X-Meta"), w.Header().Get("Age"))
	}
	h.clk.Advance(30 * time.Second)
	w = h.do(http.MethodGet, target)
	expect(t, w, 200, "HIT", "hello")
	if w.Header().Get("X-Meta") != "new" {
		t.Errorf("stored meta not updated: X-Meta=%q", w.Header().Get("X-Meta"))
	}
	if n := h.o.calls.Load(); n != 2 {
		t.Errorf("origin calls = %d, want 2", n)
	}
}

func TestRevalidate200Replaces(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.o.set(static(200, map[string]string{"Cache-Control": "max-age=1", "ETag": `"v1"`}, "one"))
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "one")
	h.clk.Advance(2 * time.Second)
	h.o.set(static(200, map[string]string{"Cache-Control": "max-age=60", "ETag": `"v2"`}, "two"))
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "two")
	expect(t, h.do(http.MethodGet, target), 200, "HIT", "two")
}

func TestStaleWithoutValidatorRefetches(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.o.set(static(200, map[string]string{"Cache-Control": "max-age=1"}, "one"))
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "one")
	h.clk.Advance(2 * time.Second)
	h.o.set(func(r *http.Request) (reply, error) {
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			return reply{}, errors.New("conditional sent without a validator")
		}
		return rep(200, map[string]string{"Cache-Control": "max-age=60"}, "two"), nil
	})
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "two")
}

func TestUnsafeMethodInvalidates(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "hello")
	expect(t, h.do(http.MethodGet, target), 200, "HIT", "hello")
	h.o.set(func(r *http.Request) (reply, error) {
		if r.Method == http.MethodPost {
			return rep(201, nil, "created"), nil
		}
		return rep(200, map[string]string{"Cache-Control": "max-age=60"}, "hello"), nil
	})
	w := h.do(http.MethodPost, target)
	expect(t, w, 201, "BYPASS", "created")
	if w.Header().Get(HeaderXCacheReason) != "unsafe-method" {
		t.Errorf("reason = %q", w.Header().Get(HeaderXCacheReason))
	}
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "hello")
	if n := h.o.calls.Load(); n != 3 {
		t.Errorf("origin calls = %d, want 3", n)
	}
	// A failed unsafe method leaves the entry alone.
	h.o.set(func(r *http.Request) (reply, error) {
		if r.Method == http.MethodDelete {
			return rep(403, nil, "no"), nil
		}
		return rep(200, map[string]string{"Cache-Control": "max-age=60"}, "hello"), nil
	})
	expect(t, h.do(http.MethodDelete, target), 403, "BYPASS", "no")
	expect(t, h.do(http.MethodGet, target), 200, "HIT", "hello")
}

func TestOnlyIfCached(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	w := h.do(http.MethodGet, target, "Cache-Control", "only-if-cached")
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", w.Code)
	}
	if h.o.calls.Load() != 0 {
		t.Error("only-if-cached must not touch the origin")
	}
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "hello")
	expect(t, h.do(http.MethodGet, target, "Cache-Control", "only-if-cached"), 200, "HIT", "hello")
	h.clk.Advance(120 * time.Second)
	if w := h.do(http.MethodGet, target, "Cache-Control", "only-if-cached"); w.Code != http.StatusGatewayTimeout {
		t.Errorf("stale + only-if-cached: status = %d, want 504", w.Code)
	}
}

func TestRequestNoStoreBypasses(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	w := h.do(http.MethodGet, target, "Cache-Control", "no-store")
	expect(t, w, 200, "BYPASS", "hello")
	if h.st.len() != 0 {
		t.Error("request no-store must not store")
	}
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "hello")
}

func TestResponseNoStoreBypasses(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.o.set(static(200, map[string]string{"Cache-Control": "no-store"}, "secret"))
	w := h.do(http.MethodGet, target)
	expect(t, w, 200, "BYPASS", "secret")
	if w.Header().Get(HeaderXCacheReason) != "response-no-store" {
		t.Errorf("reason = %q", w.Header().Get(HeaderXCacheReason))
	}
	if h.st.len() != 0 {
		t.Error("no-store response was stored")
	}
}

// cancelAfterFirstRead is an origin body that cancels the client's request
// after its first chunk and keeps delivering the rest.
type cancelAfterFirstRead struct {
	chunks [][]byte
	cancel context.CancelFunc
	once   sync.Once
}

func (b *cancelAfterFirstRead) Read(p []byte) (int, error) {
	if len(b.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.chunks[0])
	b.chunks[0] = b.chunks[0][n:]
	if len(b.chunks[0]) == 0 {
		b.chunks = b.chunks[1:]
	}
	b.once.Do(b.cancel)
	return n, nil
}

func (b *cancelAfterFirstRead) Close() error { return nil }

func TestClientDisconnectStillCommits(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	const chunk = 40 << 10
	full := bytes.Repeat([]byte("x"), 3*chunk)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.o.set(func(*http.Request) (reply, error) {
		body := &cancelAfterFirstRead{cancel: cancel}
		for i := range 3 {
			body.chunks = append(body.chunks, full[i*chunk:(i+1)*chunk])
		}
		return reply{status: http.StatusOK, hdr: map[string]string{"Cache-Control": "max-age=60"}, stream: body}, nil
	})
	w := h.doCtx(ctx, http.MethodGet, target)
	if w.Code != 200 || w.Body.Len() >= len(full) {
		t.Fatalf("client got status %d and %d bytes; expected a truncated body", w.Code, w.Body.Len())
	}
	k := cachekey.Primary(http.MethodGet, mustURL(t, target))
	if got := h.st.bodyLen(k); got != len(full) {
		t.Fatalf("stored body = %d bytes, want %d (orphan fetch must finish)", got, len(full))
	}
	w = h.do(http.MethodGet, target)
	expect(t, w, 200, "HIT", "")
	if w.Body.Len() != len(full) {
		t.Errorf("hit body = %d bytes, want %d", w.Body.Len(), len(full))
	}
	if h.o.calls.Load() != 1 {
		t.Errorf("origin calls = %d, want 1", h.o.calls.Load())
	}
}

func TestClientDisconnectAbortsPastOrphanBudget(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.OrphanMaxBytes = 1024 })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.o.set(func(*http.Request) (reply, error) {
		body := &cancelAfterFirstRead{cancel: cancel, chunks: [][]byte{bytes.Repeat([]byte("a"), 2048), bytes.Repeat([]byte("b"), 2048)}}
		return reply{status: http.StatusOK, hdr: map[string]string{"Cache-Control": "max-age=60"}, stream: body}, nil
	})
	h.doCtx(ctx, http.MethodGet, target)
	if h.st.len() != 0 {
		t.Fatal("entry committed past OrphanMaxBytes")
	}
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "")
}

func TestConcurrentMissesCollapse(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	release := make(chan struct{})
	h.o.set(func(r *http.Request) (reply, error) {
		select {
		case <-release:
		case <-r.Context().Done():
			return reply{}, r.Context().Err()
		}
		return rep(200, map[string]string{"Cache-Control": "max-age=60"}, "shared"), nil
	})
	const n = 50
	results := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = h.do(http.MethodGet, target)
		}()
	}
	time.Sleep(150 * time.Millisecond) // let the waiters join the leader
	close(release)
	wg.Wait()
	if c := h.o.calls.Load(); c != 1 {
		t.Fatalf("origin calls = %d, want 1", c)
	}
	for i, w := range results {
		if w.Code != 200 || w.Body.String() != "shared" {
			t.Fatalf("caller %d: %d %q", i, w.Code, w.Body.String())
		}
	}
	if h.rec.collapsed != n-1 {
		t.Errorf("Collapsed waiters = %d, want %d", h.rec.collapsed, n-1)
	}
	miss, hit := 0, 0
	for _, o := range h.rec.outcomeList() {
		switch o {
		case OutcomeMiss:
			miss++
		case OutcomeHit:
			hit++
		}
	}
	if miss != 1 || hit != n-1 {
		t.Errorf("outcomes: miss=%d hit=%d", miss, hit)
	}
}

func TestLeaderClientCancelDoesNotAbortWaiters(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	release := make(chan struct{})
	started := make(chan struct{})
	h.o.set(func(r *http.Request) (reply, error) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return reply{}, r.Context().Err()
		}
		return rep(200, map[string]string{"Cache-Control": "max-age=60"}, "shared"), nil
	})
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.doCtx(leaderCtx, http.MethodGet, target)
	}()
	<-started
	cancelLeader()
	waiter := make(chan *httptest.ResponseRecorder, 1)
	go func() { waiter <- h.do(http.MethodGet, target) }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	w := <-waiter
	expect(t, w, 200, "HIT", "shared")
	if h.o.calls.Load() != 1 {
		t.Errorf("origin calls = %d, want 1", h.o.calls.Load())
	}
}

func TestStaleIfError(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.o.set(static(200, map[string]string{"Cache-Control": "max-age=1, stale-if-error=60", "ETag": `"a"`}, "good"))
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "good")
	h.clk.Advance(2 * time.Second)

	h.o.set(static(500, nil, "boom"))
	w := h.do(http.MethodGet, target)
	expect(t, w, 200, "STALE", "good")
	if w.Header().Get(HeaderXCacheReason) != "stale-if-error" {
		t.Errorf("reason = %q", w.Header().Get(HeaderXCacheReason))
	}

	h.o.set(func(*http.Request) (reply, error) { return reply{}, errors.New("connection refused") })
	expect(t, h.do(http.MethodGet, target), 200, "STALE", "good")

	h.clk.Advance(120 * time.Second) // outside the window
	w = h.do(http.MethodGet, target)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), `"error"`) {
		t.Errorf("outside stale-if-error: %d %q", w.Code, w.Body.String())
	}
	h.o.set(func(*http.Request) (reply, error) { return reply{}, context.DeadlineExceeded })
	if w := h.do(http.MethodGet, "http://example.com/none"); w.Code != http.StatusGatewayTimeout {
		t.Errorf("deadline: status = %d, want 504", w.Code)
	}
}

func TestOrigin5xxWithoutStaleIsBypassed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.o.set(static(503, nil, "down"))
	w := h.do(http.MethodGet, target)
	expect(t, w, 503, "BYPASS", "down")
	if h.st.len() != 0 {
		t.Error("5xx was stored")
	}
}

func TestClientConditionalAnsweredFromCache(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	lm := "Wed, 16 Sep 2026 10:00:00 GMT"
	h.o.set(static(200, map[string]string{"Cache-Control": "max-age=60", "ETag": `"v1"`, "Last-Modified": lm}, "hello"))
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "hello")
	w := h.do(http.MethodGet, target, "If-None-Match", `W/"v1"`)
	if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Errorf("If-None-Match: %d %q", w.Code, w.Body.String())
	}
	w = h.do(http.MethodGet, target, "If-Modified-Since", lm)
	if w.Code != http.StatusNotModified {
		t.Errorf("If-Modified-Since: %d", w.Code)
	}
	expect(t, h.do(http.MethodGet, target, "If-None-Match", `"other"`), 200, "HIT", "hello")
	if h.o.calls.Load() != 1 {
		t.Errorf("origin calls = %d, want 1", h.o.calls.Load())
	}
}

func TestRange(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.o.set(static(200, map[string]string{"Cache-Control": "max-age=60", "Content-Type": "text/plain"}, "0123456789"))
	w := h.do(http.MethodGet, target, "Range", "bytes=0-3")
	expect(t, w, 200, "BYPASS", "0123456789") // miss with Range: forwarded uncached
	if w.Header().Get(HeaderXCacheReason) != "range" {
		t.Errorf("reason = %q", w.Header().Get(HeaderXCacheReason))
	}
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "0123456789")
	w = h.do(http.MethodGet, target, "Range", "bytes=0-3")
	if w.Code != http.StatusPartialContent || w.Body.String() != "0123" {
		t.Errorf("range from stored entry: %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get(HeaderXCache) != "HIT" {
		t.Errorf("X-Cache = %q", w.Header().Get(HeaderXCache))
	}
}

func TestMaxObjectBytes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.MaxObjectBytes = 4 })
	// Known length: refused up front.
	w := h.do(http.MethodGet, target)
	expect(t, w, 200, "BYPASS", "hello")
	if w.Header().Get(HeaderXCacheReason) != "too-large" {
		t.Errorf("reason = %q", w.Header().Get(HeaderXCacheReason))
	}
	// Unknown length: degrades to passthrough mid-stream, nothing stored.
	h.o.set(func(*http.Request) (reply, error) {
		return reply{status: http.StatusOK, hdr: map[string]string{"Cache-Control": "max-age=60"}, stream: io.NopCloser(strings.NewReader("streamed"))}, nil
	})
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "streamed")
	if h.st.len() != 0 {
		t.Error("oversize body was stored")
	}
}

func TestStoreWriterErrorDegrades(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.st.writerErr = store.ErrClosed
	w := h.do(http.MethodGet, target)
	expect(t, w, 200, "BYPASS", "hello")
	if w.Header().Get(HeaderXCacheReason) != string(ReasonStoreError) {
		t.Errorf("reason = %q", w.Header().Get(HeaderXCacheReason))
	}
}

func TestUnknownMethodBypasses(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.o.set(static(200, nil, "opts"))
	w := h.do(http.MethodOptions, target)
	expect(t, w, 200, "BYPASS", "opts")
	if w.Header().Get(HeaderXCacheReason) != "method" {
		t.Errorf("reason = %q", w.Header().Get(HeaderXCacheReason))
	}
}

func TestTagsAndPurge(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.PurgeVerbose = true })
	h.o.set(func(r *http.Request) (reply, error) {
		tags := map[string]string{"/a/1": "t1", "/a/2": "t1, t2", "/b/1": "t2"}[r.URL.Path]
		return rep(200, map[string]string{"Cache-Control": "max-age=60", "Cache-Tag": tags}, r.URL.Path), nil
	})
	for _, p := range []string{"/a/1", "/a/2", "/b/1"} {
		expect(t, h.do(http.MethodGet, "http://example.com"+p), 200, "MISS", p)
	}
	ctx := context.Background()
	res, err := h.c.Purge(ctx, PurgeRequest{URL: "http://example.com/a/1"})
	if err != nil || res.Purged != 1 || len(res.Keys) != 1 {
		t.Fatalf("purge url: %+v %v", res, err)
	}
	expect(t, h.do(http.MethodGet, "http://example.com/a/1"), 200, "MISS", "/a/1")

	res, err = h.c.Purge(ctx, PurgeRequest{Prefix: "http://example.com/a/"})
	if err != nil || res.Purged != 2 {
		t.Fatalf("purge prefix: %+v %v", res, err)
	}
	expect(t, h.do(http.MethodGet, "http://example.com/b/1"), 200, "HIT", "/b/1")

	res, err = h.c.Purge(ctx, PurgeRequest{Tag: "t2"})
	if err != nil || res.Purged != 1 {
		t.Fatalf("purge tag: %+v %v", res, err)
	}
	expect(t, h.do(http.MethodGet, "http://example.com/b/1"), 200, "MISS", "/b/1")

	if _, err := h.c.Purge(ctx, PurgeRequest{}); !errors.Is(err, ErrBadPurge) {
		t.Errorf("empty purge err = %v", err)
	}
	if _, err := h.c.Purge(ctx, PurgeRequest{URL: "x", Tag: "y"}); !errors.Is(err, ErrBadPurge) {
		t.Errorf("two selectors err = %v", err)
	}
	if _, err := h.c.Purge(ctx, PurgeRequest{URL: "/relative"}); !errors.Is(err, ErrBadPurge) {
		t.Errorf("relative url err = %v", err)
	}
	if h.rec.purged["url"] != 1 || h.rec.purged["prefix"] != 2 || h.rec.purged["tag"] != 1 {
		t.Errorf("Purged records = %v", h.rec.purged)
	}
}

func TestNewRebuildsTagIndexFromWalker(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.o.set(static(200, map[string]string{"Cache-Control": "max-age=60", "Surrogate-Key": "sk"}, "x"))
	expect(t, h.do(http.MethodGet, target), 200, "MISS", "x")

	// A second engine over the same store rebuilds its index by walking.
	c2, err := New(Config{Now: h.clk.Now}, h.st, h.o, &fakeRecorder{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Stop()
	res, err := c2.Purge(context.Background(), PurgeRequest{Tag: "sk"})
	if err != nil || res.Purged != 1 {
		t.Fatalf("purge after rebuild: %+v %v", res, err)
	}
}

// TestJanitorReconcilesTagIndex pins the bound on the tag index: an entry the
// store evicted on its own (LRU, budget) loses its row on the next janitor
// tick, while rows for resident entries and rows added during the walk stay.
func TestJanitorReconcilesTagIndex(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.o.set(func(r *http.Request) (reply, error) {
		return rep(200, map[string]string{"Cache-Control": "max-age=60", "Cache-Tag": "t"}, r.URL.Path), nil
	})
	for _, p := range []string{"/a/1", "/a/2", "/a/3"} {
		expect(t, h.do(http.MethodGet, "http://example.com"+p), 200, "MISS", p)
	}
	if n := h.c.TagIndexLen(); n != 3 {
		t.Fatalf("index has %d rows, want 3", n)
	}
	// The store evicts two entries behind the engine's back.
	k1 := cachekey.Primary(http.MethodGet, mustURL(t, "http://example.com/a/1"))
	k2 := cachekey.Primary(http.MethodGet, mustURL(t, "http://example.com/a/2"))
	if n, _ := h.st.Delete(context.Background(), k1, k2); n != 2 {
		t.Fatalf("deleted %d, want 2", n)
	}
	h.c.janitor(context.Background())
	if n := h.c.TagIndexLen(); n != 1 {
		t.Fatalf("index has %d rows after reconcile, want 1", n)
	}
	res, err := h.c.Purge(context.Background(), PurgeRequest{Tag: "t"})
	if err != nil || res.Purged != 1 {
		t.Fatalf("purge tag after reconcile: %+v %v", res, err)
	}
	// An empty index and a store without Walk are both no-ops.
	h.c.janitor(context.Background())
	if n := h.c.TagIndexLen(); n != 0 {
		t.Fatalf("index has %d rows, want 0", n)
	}
}

func TestRunPushesGaugesAndDrainStops(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.JanitorInterval = 5 * time.Millisecond })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.c.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.rec.mu.Lock()
		g := h.rec.gauges
		h.rec.mu.Unlock()
		if g >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if h.rec.gauges < 3 {
		t.Errorf("StoreGauges pushed %d times, want >= 3", h.rec.gauges)
	}
	// The root is gone with Run's ctx: a new miss fails fast instead of hanging.
	if w := h.do(http.MethodGet, target); w.Code != http.StatusGatewayTimeout && w.Code != http.StatusBadGateway {
		t.Errorf("after Run ended: status = %d, want 502/504", w.Code)
	}
	if err := h.c.Drain(context.Background()); err != nil {
		t.Errorf("Drain: %v", err)
	}
}

func TestNewRequiresPorts(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}, nil, nil, nil, nil); err == nil {
		t.Fatal("nil ports accepted")
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
