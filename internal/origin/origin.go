// Package origin is the upstream client: one base URL, optional Host override,
// hop-by-hop stripping both ways, a Via header, tuned transport, and a single
// retry on idempotent requests that fail with a connection error before any
// body byte was consumed.
package origin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/crowbett/edgecache/internal/rfc9111"
)

// DefaultVia is the Via value used when Config.Via is empty.
const DefaultVia = "1.1 edgecache"

// Transport defaults applied when the Config field is zero.
const (
	defaultDialTimeout     = 5 * time.Second
	defaultHeaderTimeout   = 10 * time.Second
	defaultMaxConnsPerHost = 128
	defaultMaxIdleConns    = 256
	idleConnTimeout        = 90 * time.Second
	tlsHandshakeTimeout    = 10 * time.Second
	expectContinueTimeout  = time.Second
)

// Config describes the upstream.
type Config struct {
	// Base is the upstream base URL; request paths are appended to its path.
	Base *url.URL
	// HostOverride replaces the Host header sent upstream when non-empty.
	HostOverride string
	// Via is the value appended to the Via header on both directions ("1.1 edgecache").
	Via string
	// DialTimeout, HeaderTimeout and Total bound the connect, response-header wait and whole fetch.
	DialTimeout, HeaderTimeout, Total time.Duration
	// MaxConnsPerHost and MaxIdleConns size the transport pool.
	MaxConnsPerHost, MaxIdleConns int
}

// Origin is the upstream client. It satisfies cache.Fetcher.
type Origin struct {
	cfg    Config
	log    *slog.Logger
	tr     *http.Transport
	client *http.Client
}

// New builds the client and its transport; it fails on a nil Base or one
// without scheme and host.
func New(cfg Config, log *slog.Logger) (*Origin, error) {
	if cfg.Base == nil {
		return nil, errors.New("origin: Base is required")
	}
	if cfg.Base.Scheme == "" || cfg.Base.Host == "" {
		return nil, fmt.Errorf("origin: Base %q needs a scheme and host", cfg.Base)
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.Via == "" {
		cfg.Via = DefaultVia
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	if cfg.HeaderTimeout <= 0 {
		cfg.HeaderTimeout = defaultHeaderTimeout
	}
	if cfg.MaxConnsPerHost <= 0 {
		cfg.MaxConnsPerHost = defaultMaxConnsPerHost
	}
	if cfg.MaxIdleConns <= 0 {
		cfg.MaxIdleConns = max(defaultMaxIdleConns, cfg.MaxConnsPerHost)
	}
	dialer := &net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:                 nil, // never route the upstream through an env proxy
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          cfg.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.MaxConnsPerHost,
		MaxConnsPerHost:       cfg.MaxConnsPerHost,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		ResponseHeaderTimeout: cfg.HeaderTimeout,
		// Bodies are stored as the origin sent them; the cache must not
		// transparently decode (and then have to re-encode) them.
		DisableCompression: true,
	}
	// No client Timeout: the whole-fetch budget is Config.Total, applied per
	// request through the context so a streaming body is not guillotined.
	client := &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // redirects belong to the client, not the proxy
		},
	}
	return &Origin{cfg: cfg, log: log, tr: tr, client: client}, nil
}

// Do rewrites r onto the base URL (scheme, host, path prefix, Host override),
// strips hop-by-hop fields, adds Via and X-Forwarded-*, sends it with ctx
// bounding the whole exchange (plus Config.Total), and retries once on a
// connection error for idempotent methods when no body byte was consumed. The
// response body is streamed, never buffered; closing it releases the budget.
func (o *Origin) Do(ctx context.Context, r *http.Request) (*http.Response, error) {
	cancel := context.CancelFunc(func() {})
	if o.cfg.Total > 0 {
		ctx, cancel = context.WithTimeout(ctx, o.cfg.Total)
	}
	body := trackBody(r)
	req, err := o.build(ctx, r, body)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := o.client.Do(req)
	if err != nil && o.retryable(ctx, req, body, err) {
		o.log.Debug("origin retry", slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Any("err", err))
		req2, berr := o.build(ctx, r, body)
		if berr == nil {
			resp, err = o.client.Do(req2)
		}
	}
	if err != nil {
		cancel()
		return nil, fmt.Errorf("origin: %s %s: %w", r.Method, r.URL.Path, err)
	}
	resp.Header = rfc9111.StripHopByHop(resp.Header)
	resp.Header.Add("Via", o.cfg.Via)
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// build creates the upstream request for r.
func (o *Origin) build(ctx context.Context, r *http.Request, body *countingBody) (*http.Request, error) {
	u := *o.cfg.Base
	u.Path, u.RawPath = joinPath(o.cfg.Base, r.URL)
	u.RawQuery = r.URL.RawQuery
	u.Fragment, u.RawFragment = "", ""

	var rb io.Reader
	if body != nil {
		rb = body
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, u.String(), rb)
	if err != nil {
		return nil, fmt.Errorf("origin: build request: %w", err)
	}
	if body != nil {
		req.ContentLength = r.ContentLength
		if r.ContentLength == 0 {
			req.ContentLength = -1
		}
	}
	req.Header = rfc9111.StripHopByHop(r.Header)
	req.Header.Del("Host")
	if o.cfg.HostOverride != "" {
		req.Host = o.cfg.HostOverride
	} else {
		req.Host = o.cfg.Base.Host
	}
	req.Header.Add("Via", o.cfg.Via)
	if ip, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && ip != "" {
		if prior := req.Header.Get("X-Forwarded-For"); prior != "" {
			req.Header.Set("X-Forwarded-For", prior+", "+ip)
		} else {
			req.Header.Set("X-Forwarded-For", ip)
		}
	}
	if req.Header.Get("X-Forwarded-Proto") == "" {
		proto := "http"
		if r.TLS != nil {
			proto = "https"
		}
		req.Header.Set("X-Forwarded-Proto", proto)
	}
	if req.Header.Get("X-Forwarded-Host") == "" && r.Host != "" {
		req.Header.Set("X-Forwarded-Host", r.Host)
	}
	return req, nil
}

// retryable reports whether a failed first attempt may be sent again: the
// context is still live, the method is idempotent, no request body byte was
// consumed, and the failure was a connection-level error rather than a
// timeout or a response.
func (o *Origin) retryable(ctx context.Context, req *http.Request, body *countingBody, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if !idempotent(req.Method) {
		return false
	}
	if body != nil && body.consumed() {
		return false
	}
	return isConnError(err)
}

// idempotent lists the methods RFC 9110 §9.2.2 makes safe to replay.
func idempotent(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// isConnError recognises the transport failures a fresh connection may fix.
func isConnError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "server closed idle connection") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "forcibly closed") ||
		strings.Contains(s, "broken pipe")
}

// CloseIdleConnections drops pooled connections; called at teardown stage 4.
func (o *Origin) CloseIdleConnections() {
	o.tr.CloseIdleConnections()
}

// joinPath appends the request path to the base path, keeping the request's
// raw (escaped) form when it has one.
func joinPath(base, req *url.URL) (path, rawPath string) {
	bp := strings.TrimSuffix(base.Path, "/")
	rp := req.Path
	if rp == "" {
		rp = "/"
	} else if !strings.HasPrefix(rp, "/") {
		rp = "/" + rp
	}
	path = bp + rp
	if req.RawPath != "" || base.RawPath != "" {
		brp := strings.TrimSuffix(base.EscapedPath(), "/")
		rrp := req.EscapedPath()
		if !strings.HasPrefix(rrp, "/") {
			rrp = "/" + rrp
		}
		rawPath = brp + rrp
	}
	return path, rawPath
}

// countingBody wraps a request body and remembers whether any byte was read,
// which is what decides if a retry is still safe.
type countingBody struct {
	io.ReadCloser
	read bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.read = true
	}
	return n, err
}

func (b *countingBody) consumed() bool { return b.read }

// trackBody returns nil for a bodiless request, else a counting wrapper.
func trackBody(r *http.Request) *countingBody {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	if r.ContentLength == 0 && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		return nil
	}
	return &countingBody{ReadCloser: r.Body}
}

// cancelBody releases the per-request budget when the body is closed.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}
