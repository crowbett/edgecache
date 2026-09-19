// Package http wires the HTTP router, middleware and handlers.
package http

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/crowbett/edgecache/internal/config"
)

// Readiness is toggled by the lifecycle (e.g. after DB connects, before shutdown).
type Readiness struct{ ready atomic.Bool }

// Set marks the service ready (true) or draining (false).
func (r *Readiness) Set(v bool) { r.ready.Store(v) }

// Ready reports the current readiness state.
func (r *Readiness) Ready() bool { return r.ready.Load() }

// NewRouter builds the fully wired http.Handler.
func NewRouter(logger *slog.Logger, cfg config.Config) http.Handler {
	ready := &Readiness{}
	ready.Set(true)
	return NewRouterWithReadiness(logger, cfg, ready)
}

// NewRouterWithReadiness is NewRouter with an injectable readiness gate (useful in tests).
func NewRouterWithReadiness(logger *slog.Logger, cfg config.Config, ready *Readiness) http.Handler {
	return NewRouterWithReadinessAndTracing(logger, cfg, ready)
}

// NewRouterWithReadinessAndTracing is NewRouterWithReadiness with extra otelhttp
// options threaded into the Tracing middleware (tests inject a TracerProvider).
// It is the one place the mux is built: routes come from registerRoutes, the
// middleware stack from wrap.
func NewRouterWithReadinessAndTracing(logger *slog.Logger, cfg config.Config, ready *Readiness, traceOpts ...otelhttp.Option) http.Handler {
	mux := http.NewServeMux()
	registerRoutes(mux, ready)
	return wrap(mux, logger, cfg, traceOpts...)
}

// registerRoutes is the single route table. New handlers go here (and get a row in
// TestRoutes); dependencies are threaded in as extra parameters.
func registerRoutes(mux *http.ServeMux, ready *Readiness) {
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("GET /readyz", handleReadyz(ready))
	mux.HandleFunc("GET /api/v1/ping", handlePing)
}

// wrap applies the middleware stack around h, outermost first:
//   - Tracing sits outside Logging so the log line carries trace_id.
//   - Timeout sits outside Recover: http.TimeoutHandler runs the handler on its own
//     goroutine and re-panics on the serving goroutine, so a Recover placed outside it
//     would only see the re-panic (no handler frames in the stack) and would be writing
//     to a writer the timeout handler already abandoned.
//   - Recover sits directly around TagRoute + mux so it catches on the goroutine that
//     panicked, with the real stack, and can still write a 500 through the timeout writer.
//   - TagRoute stays innermost (see its doc): nothing between it and the mux may copy
//     the request, and Recover passes r through untouched.
func wrap(h http.Handler, logger *slog.Logger, cfg config.Config, traceOpts ...otelhttp.Option) http.Handler {
	return Chain(h,
		RequestID(),
		Tracing(cfg.ServiceName, traceOpts...),
		Logging(logger),
		Timeout(cfg.RequestTimeout),
		Recover(logger),
		TagRoute(),
	)
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleReadyz(ready *Readiness) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Ready() {
			writeError(w, http.StatusServiceUnavailable, "not ready")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}

type pingResponse struct {
	Message   string    `json:"message"`
	RequestID string    `json:"request_id"`
	Time      time.Time `json:"time"`
}

func handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, pingResponse{
		Message:   "pong",
		RequestID: RequestIDFrom(r.Context()),
		Time:      time.Now().UTC(),
	})
}

// writeJSON encodes v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write json", "err", err)
	}
}

// writeError writes a JSON error envelope.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
