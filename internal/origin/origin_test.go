package origin

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func newOrigin(t *testing.T, base string, mut func(*Config)) *Origin {
	t.Helper()
	cfg := Config{Base: mustURL(t, base), HeaderTimeout: 2 * time.Second, Total: 5 * time.Second}
	if mut != nil {
		mut(&cfg)
	}
	o, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(o.CloseIdleConnections)
	return o
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}, nil); err == nil {
		t.Fatal("nil Base accepted")
	}
	if _, err := New(Config{Base: &url.URL{Path: "/x"}}, nil); err == nil {
		t.Fatal("Base without host accepted")
	}
}

func TestDoRewritesRequest(t *testing.T) {
	t.Parallel()
	var got *http.Request
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Connection", "X-Hop")
		w.Header().Set("X-Hop", "drop")
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("Via", "1.0 upstream")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	o := newOrigin(t, srv.URL+"/base/", func(c *Config) { c.HostOverride = "example.test" })

	in := httptest.NewRequest(http.MethodPut, "http://edge.local/a%20b/c?z=1&a=2", strings.NewReader("payload"))
	in.RemoteAddr = "10.0.0.7:5555"
	in.Header.Set("Connection", "X-Client-Hop")
	in.Header.Set("X-Client-Hop", "drop")
	in.Header.Set("Transfer-Encoding", "chunked")
	in.Header.Set("Accept", "text/plain")
	in.Header.Set("Via", "1.1 downstream")

	resp, err := o.Do(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if got.Host != "example.test" {
		t.Errorf("Host = %q, want example.test", got.Host)
	}
	if got.URL.RequestURI() != "/base/a%20b/c?z=1&a=2" {
		t.Errorf("request URI = %q", got.URL.RequestURI())
	}
	if got.Method != http.MethodPut || gotBody != "payload" {
		t.Errorf("method/body = %s %q", got.Method, gotBody)
	}
	if got.Header.Get("X-Client-Hop") != "" || got.Header.Get("Connection") != "" {
		t.Errorf("hop-by-hop not stripped: %v", got.Header)
	}
	if got.Header.Get("Accept") != "text/plain" {
		t.Errorf("end-to-end header lost: %v", got.Header)
	}
	if v := got.Header.Values("Via"); len(v) != 2 || v[0] != "1.1 downstream" || v[1] != DefaultVia {
		t.Errorf("Via = %v", v)
	}
	if got.Header.Get("X-Forwarded-For") != "10.0.0.7" {
		t.Errorf("X-Forwarded-For = %q", got.Header.Get("X-Forwarded-For"))
	}
	if got.Header.Get("X-Forwarded-Proto") != "http" || got.Header.Get("X-Forwarded-Host") != "edge.local" {
		t.Errorf("X-Forwarded-Proto/Host = %q/%q", got.Header.Get("X-Forwarded-Proto"), got.Header.Get("X-Forwarded-Host"))
	}

	if string(body) != "hello" || resp.StatusCode != http.StatusOK {
		t.Errorf("response = %d %q", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Hop") != "" || resp.Header.Get("Keep-Alive") != "" || resp.Header.Get("Connection") != "" {
		t.Errorf("response hop-by-hop not stripped: %v", resp.Header)
	}
	if v := resp.Header.Values("Via"); len(v) != 2 || v[1] != DefaultVia {
		t.Errorf("response Via = %v", v)
	}
	if resp.Header.Get("Content-Type") != "text/plain" {
		t.Errorf("response end-to-end header lost: %v", resp.Header)
	}
}

func TestDoDefaultHostAndNoCompression(t *testing.T) {
	t.Parallel()
	var host, ae string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, ae = r.Host, r.Header.Get("Accept-Encoding")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	o := newOrigin(t, srv.URL, nil)
	in := httptest.NewRequest(http.MethodGet, "http://edge/", http.NoBody)
	resp, err := o.Do(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if host != mustURL(t, srv.URL).Host {
		t.Errorf("Host = %q, want base host", host)
	}
	if ae != "" {
		t.Errorf("transport added Accept-Encoding %q; DisableCompression must be on", ae)
	}
}

func TestDoNoRedirectFollow(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusMovedPermanently)
	}))
	defer srv.Close()
	o := newOrigin(t, srv.URL, nil)
	resp, err := o.Do(context.Background(), httptest.NewRequest(http.MethodGet, "http://edge/", http.NoBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301 passed through", resp.StatusCode)
	}
}

// resetListener drops the first connection without a byte, then serves.
func resetOnce(t *testing.T, h http.Handler) (base string, attempts *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	attempts = new(atomic.Int32)
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if attempts.Add(1) == 1 {
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.SetLinger(0) // RST instead of FIN
				}
				c.Close()
				continue
			}
			go srv.Serve(newOneConnListener(c, ln.Addr()))
		}
	}()
	t.Cleanup(func() { ln.Close(); srv.Close() })
	return "http://" + ln.Addr().String(), attempts
}

// oneConnListener hands a single accepted conn to http.Server.Serve, then
// blocks until Close so Serve returns during srv.Close().
type oneConnListener struct {
	ch   chan net.Conn
	addr net.Addr
	done chan struct{}
	once sync.Once
}

func newOneConnListener(c net.Conn, addr net.Addr) *oneConnListener {
	ch := make(chan net.Conn, 1)
	ch <- c
	return &oneConnListener{ch: ch, addr: addr, done: make(chan struct{})}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}
func (l *oneConnListener) Addr() net.Addr { return l.addr }

func TestDoRetriesIdempotentConnError(t *testing.T) {
	t.Parallel()
	base, attempts := resetOnce(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	o := newOrigin(t, base, nil)
	resp, err := o.Do(context.Background(), httptest.NewRequest(http.MethodGet, "http://edge/x", http.NoBody))
	if err != nil {
		t.Fatalf("expected retry to succeed: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "ok" || attempts.Load() != 2 {
		t.Fatalf("body=%q attempts=%d", b, attempts.Load())
	}
}

func TestDoDoesNotRetryPost(t *testing.T) {
	t.Parallel()
	base, attempts := resetOnce(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	o := newOrigin(t, base, nil)
	_, err := o.Do(context.Background(), httptest.NewRequest(http.MethodPost, "http://edge/x", strings.NewReader("body"))) //nolint:bodyclose // the call fails: no response
	if err == nil {
		t.Fatal("POST over a reset connection should fail, not retry")
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", attempts.Load())
	}
}

func TestDoConnectionRefusedIsError(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	o := newOrigin(t, "http://"+addr, func(c *Config) { c.DialTimeout = 500 * time.Millisecond })
	_, err = o.Do(context.Background(), httptest.NewRequest(http.MethodGet, "http://edge/", http.NoBody)) //nolint:bodyclose // the call fails: no response
	if err == nil {
		t.Fatal("expected a dial error")
	}
}

func TestDoHeaderTimeout(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	o := newOrigin(t, srv.URL, func(c *Config) { c.HeaderTimeout = 50 * time.Millisecond })
	start := time.Now()
	_, err := o.Do(context.Background(), httptest.NewRequest(http.MethodGet, "http://edge/", http.NoBody)) //nolint:bodyclose // the call fails: no response
	if err == nil {
		t.Fatal("expected a header timeout")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("timeout took %s; was it retried?", time.Since(start))
	}
}

func TestDoTotalBudgetCoversBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	o := newOrigin(t, srv.URL, func(c *Config) { c.Total = 100 * time.Millisecond })
	resp, err := o.Do(context.Background(), httptest.NewRequest(http.MethodGet, "http://edge/", http.NoBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	if err == nil {
		t.Fatal("body read should fail once Total elapses")
	}
}

func TestDoCallerContextCancels(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	o := newOrigin(t, srv.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := o.Do(ctx, httptest.NewRequest(http.MethodGet, "http://edge/", http.NoBody)) //nolint:bodyclose // the call fails: no response
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestJoinPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		base, req, want string
	}{
		{"http://h", "/x", "/x"},
		{"http://h/", "/x", "/x"},
		{"http://h/base", "/x", "/base/x"},
		{"http://h/base/", "/x/y", "/base/x/y"},
		{"http://h/base", "", "/base/"},
		{"http://h", "/a%20b", "/a%20b"},
		{"http://h/p%20q", "/a%20b", "/p%20q/a%20b"},
		// Dot segments are resolved against the request path alone, never the base.
		{"http://h/base", "/../x", "/base/x"},
		{"http://h/base", "/a/../../x", "/base/x"},
		{"http://h/base", "/a/..", "/base/"},
		{"http://h/base", "/a/./b/", "/base/a/b/"},
		{"http://h/base", "/%2e%2e/x", "/base/x"},
		{"http://h/p%20q", "/..%2F%2e%2e/a%20b", "/p%20q/a%20b"},
		{"http://h", "/..", "/"},
		// No dot segment: double slashes and encoded slashes pass through untouched.
		{"http://h/base", "/a//b", "/base/a//b"},
		{"http://h/base", "/a%2Fb", "/base/a%2Fb"},
		{"http://h/base", "/.hidden/..x", "/base/.hidden/..x"},
	}
	for _, tc := range cases {
		t.Run(tc.base+"+"+tc.req, func(t *testing.T) {
			t.Parallel()
			b := mustURL(t, tc.base)
			r := mustURL(t, "http://edge"+tc.req)
			u := *b
			u.Path, u.RawPath = joinPath(b, r)
			if got := u.EscapedPath(); got != tc.want {
				t.Fatalf("joinPath = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDoCannotEscapeBasePath pins the security fix: a request path with ".."
// segments is resolved before it is joined to Base.Path, so the upstream
// request stays under the configured prefix.
func TestDoCannotEscapeBasePath(t *testing.T) {
	t.Parallel()
	var (
		mu   sync.Mutex
		seen []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.RequestURI())
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	o := newOrigin(t, srv.URL+"/public", nil)

	for _, target := range []string{
		"http://edge/../secret",
		"http://edge/a/../../secret",
		"http://edge/%2e%2e/secret",
		"http://edge/..%2Fsecret",
		"http://edge/./../secret?q=1",
	} {
		resp, err := o.Do(context.Background(), httptest.NewRequest(http.MethodGet, target, http.NoBody))
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		_ = resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	for _, uri := range seen {
		if !strings.HasPrefix(uri, "/public/secret") {
			t.Fatalf("upstream saw %q: escaped the base path", uri)
		}
	}
	if len(seen) != 5 {
		t.Fatalf("upstream saw %d requests, want 5", len(seen))
	}
}

func TestIsConnError(t *testing.T) {
	t.Parallel()
	if isConnError(context.Canceled) || isConnError(context.DeadlineExceeded) {
		t.Fatal("context errors must not be retried")
	}
	if !isConnError(io.EOF) || !isConnError(io.ErrUnexpectedEOF) {
		t.Fatal("EOF should be retried")
	}
	if !isConnError(&net.OpError{Op: "dial", Err: errors.New("refused")}) {
		t.Fatal("OpError should be retried")
	}
	if !isConnError(errors.New("http: server closed idle connection")) {
		t.Fatal("closed idle connection should be retried")
	}
	if isConnError(errors.New("something else")) {
		t.Fatal("unknown errors must not be retried")
	}
}
