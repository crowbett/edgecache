package cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/crowbett/edgecache/internal/cachekey"
)

// gatedBody is an origin body the test feeds chunk by chunk; closing ch ends
// it with io.EOF, or with err when set.
type gatedBody struct {
	ch  chan []byte
	err error
}

func newGatedBody() *gatedBody { return &gatedBody{ch: make(chan []byte)} }

func (b *gatedBody) Read(p []byte) (int, error) {
	c, ok := <-b.ch
	if !ok {
		if b.err != nil {
			return 0, b.err
		}
		return 0, io.EOF
	}
	return copy(p, c), nil
}

func (b *gatedBody) Close() error { return nil }

// liveRecorder is a ResponseWriter whose body can be observed while the
// handler is still writing it.
type liveRecorder struct {
	hdr     http.Header
	mu      sync.Mutex
	code    int
	body    bytes.Buffer
	aborted bool // the handler panicked with http.ErrAbortHandler
}

func newLive() *liveRecorder { return &liveRecorder{hdr: http.Header{}} }

func (l *liveRecorder) Header() http.Header { return l.hdr }

func (l *liveRecorder) WriteHeader(code int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.code == 0 {
		l.code = code
	}
}

func (l *liveRecorder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.code == 0 {
		l.code = http.StatusOK
	}
	return l.body.Write(p)
}

func (l *liveRecorder) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.body.Len()
}

func (l *liveRecorder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.body.String()
}

// serveLive runs one request against the harness in the background.
func (h *harness) serveLive(wg *sync.WaitGroup, ctx context.Context, w *liveRecorder, hdr ...string) {
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	wg.Go(func() {
		defer func() {
			if rec := recover(); rec != nil {
				if rec != http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity per net/http docs
					panic(rec)
				}
				w.mu.Lock()
				w.aborted = true
				w.mu.Unlock()
			}
		}()
		h.h.ServeHTTP(w, r)
	})
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

var cacheable = map[string]string{"Cache-Control": "max-age=60"}

func TestCollapsedWaitersStreamBeforeCommit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.CollapseStreamBytes = 1 << 20 })
	body := newGatedBody()
	h.o.set(func(*http.Request) (reply, error) {
		return reply{status: http.StatusOK, hdr: cacheable, stream: body}, nil
	})
	var wg sync.WaitGroup
	leader := newLive()
	h.serveLive(&wg, context.Background(), leader)
	waitUntil(t, "origin call", func() bool { return h.o.calls.Load() == 1 })
	first, second := bytes.Repeat([]byte("A"), 1000), bytes.Repeat([]byte("B"), 1000)
	body.ch <- first
	waitUntil(t, "leader's first chunk", func() bool { return leader.len() == len(first) })

	// The origin is still holding the rest of the body: waiters joining now
	// get the first chunk from the stream, not after the commit.
	const n = 8
	waiters := make([]*liveRecorder, n)
	for i := range waiters {
		waiters[i] = newLive()
		h.serveLive(&wg, context.Background(), waiters[i])
	}
	for i, w := range waiters {
		waitUntil(t, "waiter first chunk", func() bool { return w.len() == len(first) })
		if h.st.len() != 0 {
			t.Fatalf("waiter %d: entry committed before the body finished", i)
		}
	}
	body.ch <- second
	close(body.ch)
	wg.Wait()

	want := string(first) + string(second)
	if leader.String() != want || leader.hdr.Get(HeaderXCache) != "MISS" {
		t.Errorf("leader: X-Cache %q, %d bytes", leader.hdr.Get(HeaderXCache), leader.len())
	}
	for i, w := range waiters {
		if w.code != http.StatusOK || w.String() != want {
			t.Errorf("waiter %d: status %d, %d bytes", i, w.code, w.len())
		}
		if got := w.hdr.Get(HeaderXCache); got != "HIT" {
			t.Errorf("waiter %d: X-Cache = %q, want HIT", i, got)
		}
		if cl := w.hdr.Get("Content-Length"); cl != "" {
			t.Errorf("waiter %d: Content-Length %q on a body of unknown length", i, cl)
		}
	}
	if c := h.o.calls.Load(); c != 1 {
		t.Errorf("origin calls = %d, want 1", c)
	}
	if h.rec.collapsed != n {
		t.Errorf("Collapsed waiters = %d, want %d", h.rec.collapsed, n)
	}
	k := cachekey.Primary(http.MethodGet, mustURL(t, target))
	if got := h.st.bodyLen(k); got != len(want) {
		t.Errorf("stored body = %d bytes, want %d", got, len(want))
	}
}

func TestCollapseStreamDisabledWaitsForCommit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil) // CollapseStreamBytes 0
	body := newGatedBody()
	h.o.set(func(*http.Request) (reply, error) {
		return reply{status: http.StatusOK, hdr: cacheable, stream: body}, nil
	})
	var wg sync.WaitGroup
	leader := newLive()
	h.serveLive(&wg, context.Background(), leader)
	waitUntil(t, "origin call", func() bool { return h.o.calls.Load() == 1 })
	body.ch <- []byte("first")
	waitUntil(t, "leader's first chunk", func() bool { return leader.len() == 5 })
	waiter := newLive()
	h.serveLive(&wg, context.Background(), waiter)
	time.Sleep(50 * time.Millisecond)
	if n := waiter.len(); n != 0 {
		t.Fatalf("waiter got %d bytes before the commit with streaming off", n)
	}
	body.ch <- []byte("second")
	close(body.ch)
	wg.Wait()
	if waiter.String() != "firstsecond" || waiter.hdr.Get(HeaderXCache) != "HIT" {
		t.Errorf("waiter: X-Cache %q body %q", waiter.hdr.Get(HeaderXCache), waiter.String())
	}
}

func TestStreamOutlivesLeaderClient(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) {
		c.CollapseStreamBytes = 1 << 20
		c.OrphanFetch = false // the leader alone would stop at its client's exit
	})
	body := newGatedBody()
	h.o.set(func(*http.Request) (reply, error) {
		return reply{status: http.StatusOK, hdr: cacheable, stream: body}, nil
	})
	var wg sync.WaitGroup
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	leader, waiter := newLive(), newLive()
	h.serveLive(&wg, leaderCtx, leader)
	waitUntil(t, "origin call", func() bool { return h.o.calls.Load() == 1 })
	body.ch <- []byte("one,")
	waitUntil(t, "leader's first chunk", func() bool { return leader.len() == 4 })
	h.serveLive(&wg, context.Background(), waiter)
	waitUntil(t, "waiter first chunk", func() bool { return waiter.len() == 4 })

	cancelLeader()
	body.ch <- []byte("two,")
	body.ch <- []byte("three")
	close(body.ch)
	wg.Wait()
	if got := waiter.String(); got != "one,two,three" {
		t.Fatalf("waiter body = %q, want the whole body", got)
	}
	if leader.aborted || waiter.aborted {
		t.Errorf("aborted: leader %v waiter %v; a client leaving is not an origin failure", leader.aborted, waiter.aborted)
	}
	if n := h.st.len(); n != 0 {
		t.Errorf("%d entries stored with orphan fetch off", n)
	}
}

func TestStreamOriginErrorTruncatesWaiters(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.CollapseStreamBytes = 1 << 20 })
	body := newGatedBody()
	body.err = errors.New("connection reset by origin")
	h.o.set(func(*http.Request) (reply, error) {
		return reply{status: http.StatusOK, hdr: cacheable, stream: body}, nil
	})
	var wg sync.WaitGroup
	leader, waiter := newLive(), newLive()
	h.serveLive(&wg, context.Background(), leader)
	waitUntil(t, "origin call", func() bool { return h.o.calls.Load() == 1 })
	body.ch <- []byte("partial")
	waitUntil(t, "leader's first chunk", func() bool { return leader.len() == 7 })
	h.serveLive(&wg, context.Background(), waiter)
	waitUntil(t, "waiter first chunk", func() bool { return waiter.len() == 7 })
	close(body.ch)
	wg.Wait()
	if waiter.String() != "partial" || leader.String() != "partial" {
		t.Errorf("leader %q waiter %q, want both truncated at the failure", leader.String(), waiter.String())
	}
	if !leader.aborted || !waiter.aborted {
		t.Errorf("aborted: leader %v waiter %v; a short body must abort the connection", leader.aborted, waiter.aborted)
	}
	if n := h.st.len(); n != 0 {
		t.Errorf("%d entries stored from a failed body", n)
	}
}

func TestStreamVaryMismatchFetchesOwnVariant(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.CollapseStreamBytes = 1 << 20 })
	body := newGatedBody()
	hdr := map[string]string{"Cache-Control": "max-age=60", "Vary": "Accept-Language"}
	h.o.set(func(r *http.Request) (reply, error) {
		if r.Header.Get("Accept-Language") == "fr" {
			return rep(http.StatusOK, hdr, "bonjour"), nil
		}
		return reply{status: http.StatusOK, hdr: hdr, stream: body}, nil
	})
	var wg sync.WaitGroup
	leader, waiter := newLive(), newLive()
	h.serveLive(&wg, context.Background(), leader, "Accept-Language", "en")
	waitUntil(t, "origin call", func() bool { return h.o.calls.Load() == 1 })
	body.ch <- []byte("hello")
	waitUntil(t, "leader's first chunk", func() bool { return leader.len() == 5 })
	h.serveLive(&wg, context.Background(), waiter, "Accept-Language", "fr")
	time.Sleep(20 * time.Millisecond) // let it join the leader
	close(body.ch)
	wg.Wait()
	if got := waiter.String(); got != "bonjour" {
		t.Fatalf("fr waiter got %q: another variant's body was shared", got)
	}
	if got := leader.String(); got != "hello" {
		t.Errorf("leader got %q", got)
	}
}

func TestStreamWaiterConditionalGets304(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.CollapseStreamBytes = 1 << 20 })
	body := newGatedBody()
	h.o.set(func(*http.Request) (reply, error) {
		return reply{status: http.StatusOK, hdr: map[string]string{"Cache-Control": "max-age=60", "ETag": `"v1"`}, stream: body}, nil
	})
	var wg sync.WaitGroup
	leader, waiter := newLive(), newLive()
	h.serveLive(&wg, context.Background(), leader)
	waitUntil(t, "origin call", func() bool { return h.o.calls.Load() == 1 })
	body.ch <- []byte("payload")
	waitUntil(t, "leader's first chunk", func() bool { return leader.len() == 7 })
	h.serveLive(&wg, context.Background(), waiter, "If-None-Match", `"v1"`)
	waitUntil(t, "waiter status", func() bool {
		waiter.mu.Lock()
		defer waiter.mu.Unlock()
		return waiter.code != 0
	})
	close(body.ch)
	wg.Wait()
	if waiter.code != http.StatusNotModified || waiter.len() != 0 {
		t.Errorf("waiter: status %d, %d bytes; want 304 with no body", waiter.code, waiter.len())
	}
}
