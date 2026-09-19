package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Middleware wraps an http.Handler with additional behaviour.
type Middleware func(http.Handler) http.Handler

// Chain composes middlewares so that the first in the list is the outermost.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

type ctxKey int

const requestIDKey ctxKey = iota

// RequestIDHeader is the header used to propagate request identifiers.
const RequestIDHeader = "X-Request-Id"

// RequestIDFrom returns the request id stored in ctx, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// maxRequestIDLen bounds a client-supplied X-Request-Id; anything longer is replaced.
const maxRequestIDLen = 128

// RequestID reuses an incoming X-Request-Id when it is well-formed (see validRequestID),
// otherwise generates a new one; the id is stored in the context and echoed on the response.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(RequestIDHeader)
			if !validRequestID(id) {
				id = newRequestID()
			}
			w.Header().Set(RequestIDHeader, id)
			ctx := context.WithValue(r.Context(), requestIDKey, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// validRequestID accepts 1..maxRequestIDLen bytes of visible ASCII (0x21-0x7E): no
// whitespace, control characters or non-ASCII, so the id is safe to echo in headers,
// log lines and span attributes without escaping and cannot bloat them.
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UTC().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b[:])
}

// statusWriter captures the status code and bytes written for logging.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer (Flush, Hijack, etc).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Logging emits one structured log line per request.
func Logging(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r)

			level := slog.LevelInfo
			switch {
			case sw.status >= 500:
				level = slog.LevelError
			case sw.status >= 400:
				level = slog.LevelWarn
			}
			logger.LogAttrs(r.Context(), level, "request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", sw.status),
				slog.Int("bytes", sw.bytes),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", RequestIDFrom(r.Context())),
				slog.String("remote", r.RemoteAddr),
				slog.String("user_agent", r.UserAgent()),
			)
		})
	}
}

// Recover converts panics into 500 responses and logs the stack trace.
func Recover(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity per net/http docs
						panic(rec)
					}
					logger.ErrorContext(r.Context(), "panic recovered",
						slog.Any("panic", rec),
						slog.String("request_id", RequestIDFrom(r.Context())),
						slog.String("stack", string(debug.Stack())),
					)
					writeError(w, http.StatusInternalServerError, "internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Tracing wraps the handler with otelhttp: one server span per request (named after
// the ServeMux pattern, e.g. "GET /api/v1/ping") plus the http.server.request.duration
// histogram, both pushed through the global providers set up by internal/otel. With
// OTel disabled the globals are no-ops and this costs almost nothing.
//
// Probes (/healthz, /readyz) are filtered out so they neither trace nor skew metrics.
// Extra otelhttp options (e.g. WithTracerProvider in tests) are appended last.
func Tracing(serviceName string, opts ...otelhttp.Option) Middleware {
	base := []otelhttp.Option{
		otelhttp.WithServerName(serviceName),
		otelhttp.WithSpanNameFormatter(spanName),
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/healthz" && r.URL.Path != "/readyz"
		}),
	}
	return func(next http.Handler) http.Handler {
		// Tag the span with the request id so logs (which carry both) and traces line up.
		tagged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if id := RequestIDFrom(r.Context()); id != "" {
				trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("http.request.id", id))
			}
			next.ServeHTTP(w, r)
		})
		return otelhttp.NewHandler(tagged, "http.server", append(base, opts...)...)
	}
}

// spanName follows the HTTP semconv: "{method} {route}" once the mux has matched a
// pattern (otelhttp re-runs the formatter after ServeHTTP), otherwise just "{method}".
func spanName(_ string, r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern // already "METHOD /path" for method patterns
	}
	return r.Method
}

// TagRoute must be the innermost middleware (directly around the mux). ServeMux sets
// r.Pattern on the request it receives, but http.TimeoutHandler (and any middleware
// calling r.WithContext) hands the mux a copy, so otelhttp's original request never sees
// it. This reads the pattern where it is visible and pushes it up through the context:
// the server span gets its "{method} {route}" name and the metric labeler gets http.route.
// Recover sits directly outside it and passes r through untouched, so the pattern is
// still visible here; the deferred tagging also runs while a panic is unwinding.
func TagRoute() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { // deferred: the pattern is set before the handler runs, even if it panics
				if r.Pattern == "" {
					return
				}
				span := trace.SpanFromContext(r.Context())
				span.SetName(spanName("", r))
				if route := routeFromPattern(r.Pattern); route != "" {
					attr := semconv.HTTPRoute(route)
					span.SetAttributes(attr)
					if l, ok := otelhttp.LabelerFromContext(r.Context()); ok {
						l.Add(attr)
					}
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// routeFromPattern strips the optional "METHOD " / "host" prefix: "GET /a/{id}" -> "/a/{id}".
func routeFromPattern(pattern string) string {
	if i := strings.IndexByte(pattern, '/'); i >= 0 {
		return pattern[i:]
	}
	return ""
}

// Timeout bounds handler execution; slow handlers get a 503.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.TimeoutHandler(next, d, `{"error":"request timed out"}`)
	}
}
