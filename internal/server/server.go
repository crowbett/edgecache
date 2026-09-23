// Package server owns the two listeners and their route tables: the proxy
// listener (catch-all cache handler + PURGE with X-Purge-Token) built without
// http.TimeoutHandler, and the loopback admin listener (/healthz, /readyz,
// /metrics, /admin/stats, /admin/purge, /debug/pprof) behind the template
// middleware stack including Timeout.
//
// Purge authorisation is the same on both listeners: when a token is
// configured the request must carry it in X-Purge-Token (constant-time
// compare; missing -> 401, wrong -> 403); with no token configured only
// loopback peers may purge (anything else -> 403). Errors go to clients as
// stable JSON strings; internals are logged with the request id.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof" // G108 does not apply: mounted on the loopback admin mux only, DefaultServeMux is never served
	"strings"
	"time"

	"github.com/crowbett/edgecache/internal/cache"
	apihttp "github.com/crowbett/edgecache/internal/http"
	"github.com/crowbett/edgecache/internal/store"
	"github.com/crowbett/edgecache/internal/trustproxy"
)

// PurgeHeader is the request header carrying the PURGE credential on the proxy listener.
const PurgeHeader = "X-Purge-Token"

// MethodPurge is the proxy-listener purge method (not in net/http's list).
const MethodPurge = "PURGE"

// Listener timeouts. The proxy has no WriteTimeout: responses stream from a
// slow origin and their budget is the origin context, not the socket.
const (
	proxyReadHeaderTimeout = 10 * time.Second
	proxyIdleTimeout       = 120 * time.Second
	adminReadHeaderTimeout = 5 * time.Second
	adminReadTimeout       = 15 * time.Second
	adminIdleTimeout       = 60 * time.Second
	adminWriteSlack        = 5 * time.Second
	maxPurgeBody           = 64 << 10
)

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
	// TrustedProxies are the peers whose X-Forwarded-Proto is honoured when
	// PURGE rebuilds the URL it addresses (the engine applies the same rule).
	TrustedProxies trustproxy.List
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

	// Version is reported by /admin/stats (main's -ldflags -X main.version).
	Version string
	// StoreStats is the store's per-tier gauges for /admin/stats (cache.Store.Stats); optional.
	StoreStats func() map[string]store.Stats
	// InFlight reports collapse leaders in flight (collapse.Group.InFlight); optional.
	InFlight func() int
	// TagIndexLen reports how many keys the tag index holds (tagindex.Index.Len); optional.
	TagIndexLen func() int
	// ConfigSummary is the non-secret config subset main wants shown in /admin/stats; optional.
	ConfigSummary map[string]any
}

// NewProxyHandler builds the proxy listener's handler: RequestID -> Tracing ->
// Logging -> Recover -> {PURGE, Proxy}; no Timeout (streaming lesson from aigw).
func NewProxyHandler(cfg Config, d Deps) http.Handler {
	log := d.logger()
	proxy := d.Proxy
	if proxy == nil {
		proxy = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusBadGateway, "no proxy handler")
		})
	}
	purge := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status, msg := authorize(cfg, r); status != 0 {
			writeError(w, status, msg)
			return
		}
		doPurge(w, r, d.Purger, log, cache.PurgeRequest{URL: requestURL(r, cfg.TrustedProxies)}, "url")
	})
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == MethodPurge {
			purge.ServeHTTP(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})
	return apihttp.Chain(inner,
		apihttp.RequestID(),
		apihttp.Tracing(cfg.ServiceName),
		apihttp.Logging(log),
		apihttp.Recover(log),
	)
}

// NewAdminHandler builds the admin listener's handler through the full template
// middleware stack (with Timeout): probes, metrics, stats, purge, pprof.
func NewAdminHandler(cfg Config, d Deps) http.Handler {
	log := d.logger()
	started := time.Now()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if d.Ready != nil && !d.Ready.Ready() {
			writeError(w, http.StatusServiceUnavailable, "not ready")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	if d.Metrics != nil {
		mux.Handle("GET /metrics", d.Metrics)
	} else {
		mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusNotFound, "metrics not configured")
		})
	}
	mux.HandleFunc("GET /admin/stats", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, buildStats(cfg, d, started))
	})
	mux.HandleFunc("POST /admin/purge", func(w http.ResponseWriter, r *http.Request) {
		if status, msg := authorize(cfg, r); status != 0 {
			writeError(w, status, msg)
			return
		}
		var body struct {
			URL    string `json:"url"`
			Prefix string `json:"prefix"`
			Tag    string `json:"tag"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPurgeBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "bad request")
			return
		}
		req := cache.PurgeRequest{URL: body.URL, Prefix: body.Prefix, Tag: body.Tag}
		doPurge(w, r, d.Purger, log, req, purgeKind(req))
	})

	// pprof is admin-only: the loopback listener, never the proxy port.
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("POST /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)

	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return apihttp.Chain(mux,
		apihttp.RequestID(),
		apihttp.Tracing(cfg.ServiceName),
		apihttp.Logging(log),
		apihttp.Timeout(timeout),
		apihttp.Recover(log),
		apihttp.TagRoute(),
	)
}

// NewServers returns the two configured *http.Server values (proxy, admin) with
// timeouts set; main starts and stops them in its numbered stages.
func NewServers(cfg Config, d Deps) (proxy, admin *http.Server) {
	log := d.logger()
	errLog := slog.NewLogLogger(log.Handler(), slog.LevelError)
	proxy = &http.Server{
		Addr:              cfg.Addr,
		Handler:           NewProxyHandler(cfg, d),
		ReadHeaderTimeout: proxyReadHeaderTimeout,
		IdleTimeout:       proxyIdleTimeout,
		ErrorLog:          errLog,
	}
	writeTimeout := cfg.RequestTimeout + adminWriteSlack
	if cfg.RequestTimeout <= 0 {
		writeTimeout = 60*time.Second + adminWriteSlack
	}
	admin = &http.Server{
		Addr:              cfg.AdminAddr,
		Handler:           NewAdminHandler(cfg, d),
		ReadHeaderTimeout: adminReadHeaderTimeout,
		ReadTimeout:       adminReadTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       adminIdleTimeout,
		ErrorLog:          errLog,
	}
	return proxy, admin
}

func (d Deps) logger() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// authorize applies the purge rule and returns (0, "") when the request may
// purge, otherwise the status and stable error string to send.
func authorize(cfg Config, r *http.Request) (int, string) {
	if cfg.PurgeToken != "" {
		got := r.Header.Get(PurgeHeader)
		if got == "" {
			return http.StatusUnauthorized, "missing " + PurgeHeader
		}
		if !tokenEqual(got, cfg.PurgeToken) {
			return http.StatusForbidden, "bad " + PurgeHeader
		}
		return 0, ""
	}
	if !isLoopbackAddr(r.RemoteAddr) {
		return http.StatusForbidden, "purge requires " + PurgeHeader + " or a loopback peer"
	}
	return 0, ""
}

// tokenEqual compares a presented credential with the configured one in
// constant time. Both sides are hashed first so the comparison takes the same
// time whatever the lengths: subtle.ConstantTimeCompare alone returns early on
// a length mismatch, which would let a caller learn the token's length.
func tokenEqual(got, want string) bool {
	g, w := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}

// doPurge runs one purge through the engine and writes {"purged":N[,"keys":[...]]}.
func doPurge(w http.ResponseWriter, r *http.Request, p Purger, log *slog.Logger, req cache.PurgeRequest, kind string) {
	if p == nil {
		writeError(w, http.StatusNotImplemented, "purge not configured")
		return
	}
	res, err := p.Purge(r.Context(), req)
	if err != nil {
		if errors.Is(err, cache.ErrBadPurge) {
			writeError(w, http.StatusBadRequest, "purge needs exactly one of url, prefix, tag")
			return
		}
		log.ErrorContext(r.Context(), "purge failed",
			slog.String("kind", kind),
			slog.Any("err", err),
			slog.String("request_id", apihttp.RequestIDFrom(r.Context())),
		)
		writeError(w, http.StatusInternalServerError, "purge failed")
		return
	}
	type purgeResponse struct {
		Purged int      `json:"purged"`
		Kind   string   `json:"kind"`
		Keys   []string `json:"keys,omitempty"`
	}
	writeJSON(w, http.StatusOK, purgeResponse{Purged: res.Purged, Kind: kind, Keys: res.Keys})
}

// purgeKind names the selector for logs and the response; the engine enforces
// "exactly one".
func purgeKind(req cache.PurgeRequest) string {
	switch {
	case req.Tag != "":
		return "tag"
	case req.Prefix != "":
		return "prefix"
	default:
		return "url"
	}
}

// requestURL rebuilds the absolute URL a proxy-listener request addressed
// (scheme://host/path?query), which is what the engine keys on. As in the
// engine, X-Forwarded-Proto counts only from a trusted peer.
func requestURL(r *http.Request, trusted trustproxy.List) string {
	if r.URL.IsAbs() {
		return r.URL.String()
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); (p == "https" || p == "http") && trusted.Trusts(r.RemoteAddr) {
		scheme = p
	}
	return scheme + "://" + r.Host + r.URL.RequestURI()
}

// isLoopbackAddr reports whether a RemoteAddr ("ip:port") is a loopback peer.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Stats is the JSON shape of GET /admin/stats.
type Stats struct {
	Service          string                 `json:"service"`
	Version          string                 `json:"version"`
	UptimeSeconds    float64                `json:"uptime_seconds"`
	Store            map[string]store.Stats `json:"store"`
	CollapseInFlight int                    `json:"collapse_inflight"`
	TagIndexLen      int                    `json:"tagindex_len"`
	Config           map[string]any         `json:"config"`
	Metrics          json.RawMessage        `json:"metrics"`
}

func buildStats(cfg Config, d Deps, started time.Time) Stats {
	s := Stats{
		Service:       cfg.ServiceName,
		Version:       d.Version,
		UptimeSeconds: time.Since(started).Seconds(),
		Store:         map[string]store.Stats{},
		Config:        map[string]any{},
		Metrics:       json.RawMessage("null"),
	}
	if s.Version == "" {
		s.Version = "dev"
	}
	if d.StoreStats != nil {
		for k, v := range d.StoreStats() {
			s.Store[k] = v
		}
	}
	if d.InFlight != nil {
		s.CollapseInFlight = d.InFlight()
	}
	if d.TagIndexLen != nil {
		s.TagIndexLen = d.TagIndexLen()
	}
	for k, v := range d.ConfigSummary {
		s.Config[k] = v
	}
	s.Config["addr"] = cfg.Addr
	s.Config["admin_addr"] = cfg.AdminAddr
	s.Config["purge_token_set"] = cfg.PurgeToken != ""
	s.Config["request_timeout"] = cfg.RequestTimeout.String()
	s.Config["shutdown_timeout"] = cfg.ShutdownTimeout.String()
	s.Config["trusted_proxies"] = cfg.TrustedProxies.String()
	if d.Stats != nil {
		if raw := d.Stats(); json.Valid(raw) {
			s.Metrics = raw
		}
	}
	return s
}

// writeJSON encodes v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		slog.Debug("write json", slog.Any("err", err))
	}
}

// writeError writes a JSON error envelope with a stable message.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
