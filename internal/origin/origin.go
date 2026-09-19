// Package origin is the upstream client: one base URL, optional Host override,
// hop-by-hop stripping both ways, a Via header, tuned transport, and a single
// retry on idempotent requests that fail with a connection error before any
// body byte was consumed.
//
// STUB written by the integrator to freeze the contract; workstream C owns this
// package and replaces the bodies, keeping every exported signature.
package origin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"
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
	cfg Config
	log *slog.Logger
}

// New builds the client and its transport; it fails on a nil Base.
func New(cfg Config, log *slog.Logger) (*Origin, error) {
	if cfg.Base == nil {
		return nil, errors.New("origin: Base is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Origin{cfg: cfg, log: log}, nil
}

// Do rewrites r onto the base URL (scheme, host, path prefix, Host override),
// strips hop-by-hop fields, adds Via, sends it with ctx bounding the whole
// exchange, and retries once on a connection error for idempotent methods when
// no body was consumed. The response body is streamed, never buffered.
func (o *Origin) Do(ctx context.Context, r *http.Request) (*http.Response, error) {
	_, _, _ = o, ctx, r
	return nil, errors.New("stub: origin.Origin.Do")
}

// CloseIdleConnections drops pooled connections; called at teardown stage 4.
func (o *Origin) CloseIdleConnections() {
	_ = o
}
