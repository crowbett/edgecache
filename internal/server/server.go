// Package server owns the two listeners and their route tables: the proxy
// listener (catch-all cache handler + PURGE with X-Purge-Token) built without
// http.TimeoutHandler, and the loopback admin listener (/healthz, /readyz,
// /metrics, /admin/stats, /admin/purge, /debug/pprof) behind the template
// middleware stack including Timeout.
//
// STUB written by the integrator to freeze the contract; workstream D owns this
// package and replaces the bodies, keeping every exported signature.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/crowbett/edgecache/internal/cache"
	apihttp "github.com/crowbett/edgecache/internal/http"
)

// PurgeHeader is the request header carrying the PURGE credential on the proxy listener.
const PurgeHeader = "X-Purge-Token"

// Config is the listener-level subset of config.Config (main maps it).
type Config struct {
	ServiceName string
	Addr        string // proxy listener
	AdminAddr   string // admin listener
	// PurgeToken enables PURGE on the proxy listener when non-empty (constant-time compare).
	PurgeToken string
	// RequestTimeout bounds admin handlers only.
	RequestTimeout time.Duration
	// ShutdownTimeout is the proxy drain budget the server reports in /admin/stats.
	ShutdownTimeout time.Duration
}

// Purger is what the purge endpoints need from the engine; *cache.Cache satisfies it.
type Purger interface {
	Purge(ctx context.Context, req cache.PurgeRequest) (cache.PurgeResult, error)
}

// Deps are the collaborators the route tables mount.
type Deps struct {
	// Proxy is the catch-all handler (cache.Handler()).
	Proxy http.Handler
	// Purger serves PURGE and POST /admin/purge.
	Purger Purger
	// Metrics serves GET /metrics.
	Metrics http.Handler
	// Stats renders GET /admin/stats (metrics.Registry.SnapshotJSON).
	Stats func() []byte
	// Ready gates /readyz; main flips it at teardown stage 1.
	Ready *apihttp.Readiness
	Log   *slog.Logger
}

// NewProxyHandler builds the proxy listener's handler: RequestID -> Tracing ->
// Logging -> Recover -> {PURGE, Proxy}; no Timeout (streaming lesson from aigw).
func NewProxyHandler(cfg Config, d Deps) http.Handler {
	_ = cfg
	if d.Proxy != nil {
		return d.Proxy
	}
	return stub("server.NewProxyHandler")
}

// NewAdminHandler builds the admin listener's handler through the full template
// middleware stack (with Timeout): probes, metrics, stats, purge, pprof.
func NewAdminHandler(cfg Config, d Deps) http.Handler {
	_, _ = cfg, d
	return stub("server.NewAdminHandler")
}

// NewServers returns the two configured *http.Server values (proxy, admin) with
// timeouts set; main starts and stops them in its numbered stages.
func NewServers(cfg Config, d Deps) (proxy, admin *http.Server) {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	errLog := slog.NewLogLogger(log.Handler(), slog.LevelError)
	proxy = &http.Server{
		Addr:              cfg.Addr,
		Handler:           NewProxyHandler(cfg, d),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          errLog,
	}
	admin = &http.Server{
		Addr:              cfg.AdminAddr,
		Handler:           NewAdminHandler(cfg, d),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      cfg.RequestTimeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          errLog,
	}
	return proxy, admin
}

func stub(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "stub: "+name, http.StatusNotImplemented)
	})
}
