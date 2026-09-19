package http

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/crowbett/edgecache/internal/config"
)

func testRouter(t *testing.T, ready bool) http.Handler {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	cfg := config.Config{RequestTimeout: 2 * time.Second}
	r := &Readiness{}
	r.Set(ready)
	return NewRouterWithReadiness(logger, cfg, r)
}

func TestRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		method     string
		path       string
		ready      bool
		wantStatus int
		wantBody   map[string]string // subset match on string fields
	}{
		{"healthz", http.MethodGet, "/healthz", true, http.StatusOK, map[string]string{"status": "ok"}},
		{"readyz ok", http.MethodGet, "/readyz", true, http.StatusOK, map[string]string{"status": "ready"}},
		{"readyz draining", http.MethodGet, "/readyz", false, http.StatusServiceUnavailable, map[string]string{"error": "not ready"}},
		{"ping", http.MethodGet, "/api/v1/ping", true, http.StatusOK, map[string]string{"message": "pong"}},
		{"ping wrong method", http.MethodPost, "/api/v1/ping", true, http.StatusMethodNotAllowed, nil},
		{"not found", http.MethodGet, "/nope", true, http.StatusNotFound, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := testRouter(t, tt.ready)
			req := httptest.NewRequest(tt.method, tt.path, http.NoBody)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if rec.Header().Get(RequestIDHeader) == "" {
				t.Errorf("missing %s header", RequestIDHeader)
			}
			if tt.wantBody == nil {
				return
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode body: %v (%q)", err, rec.Body.String())
			}
			for k, want := range tt.wantBody {
				if got[k] != want {
					t.Errorf("body[%q] = %v, want %q", k, got[k], want)
				}
			}
		})
	}
}

func TestRequestIDPropagation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string // incoming X-Request-Id ("" = header absent)
		keep bool   // true: echoed as-is; false: replaced with a generated id
	}{
		{"absent", "", false},
		{"simple", "abc-123", true},
		{"uuid-ish", "550e8400-e29b-41d4-a716-446655440000", true},
		{"all visible ascii", "!\"#$%&'()*+,-./09:;<=>?@AZ[\\]^_`az{|}~", true},
		{"max length", strings.Repeat("a", maxRequestIDLen), true},
		{"too long", strings.Repeat("a", maxRequestIDLen+1), false},
		{"space", "abc 123", false},
		{"tab", "abc\t123", false},
		{"control", "abc\x01123", false},
		{"del", "abc\x7f123", false},
		{"non-ascii", "abc-é-123", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := testRouter(t, true)
			req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", http.NoBody)
			if tt.in != "" {
				req.Header.Set(RequestIDHeader, tt.in)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			got := rec.Header().Get(RequestIDHeader)
			var body pingResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.RequestID != got {
				t.Errorf("body request_id = %q, header = %q; want equal", body.RequestID, got)
			}
			if tt.keep {
				if got != tt.in {
					t.Errorf("response header = %q, want %q echoed", got, tt.in)
				}
				return
			}
			if got == tt.in || !validRequestID(got) {
				t.Errorf("response header = %q, want a fresh generated id (input %q)", got, tt.in)
			}
		})
	}
}

func TestRecoverMiddleware(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}), RequestID(), Recover(logger))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestPanicThroughFullStack drives a panicking handler through the production
// middleware order (wrap) and checks the client gets a 500 with the request id, and
// the log line carries the panic value and the panicking handler's own frame - i.e.
// Recover ran inside Timeout, on the goroutine that panicked, not on the re-panic.
func TestPanicThroughFullStack(t *testing.T) {
	t.Parallel()
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	cfg := config.Config{ServiceName: "api-test", RequestTimeout: 2 * time.Second}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", panickingHandler)
	h := wrap(mux, logger, cfg)

	req := httptest.NewRequest(http.MethodGet, "/boom", http.NoBody)
	req.Header.Set(RequestIDHeader, "panic-req-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(RequestIDHeader); got != "panic-req-1" {
		t.Errorf("%s = %q, want panic-req-1", RequestIDHeader, got)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v (%q)", err, rec.Body.String())
	}
	if body["error"] != "internal server error" {
		t.Errorf("body = %v, want error=internal server error", body)
	}

	// One "panic recovered" line (from Recover) and one "request" line (from Logging).
	var panicLine map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logBuf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if rec["msg"] == "panic recovered" {
			panicLine = rec
		}
	}
	if panicLine == nil {
		t.Fatalf("no 'panic recovered' log line in:\n%s", logBuf.String())
	}
	if panicLine["panic"] != "kaboom-7f3a" {
		t.Errorf("log panic = %v, want kaboom-7f3a", panicLine["panic"])
	}
	if panicLine["request_id"] != "panic-req-1" {
		t.Errorf("log request_id = %v, want panic-req-1", panicLine["request_id"])
	}
	// The handler frame is only on the goroutine that panicked. If Recover sat outside
	// Timeout it would catch http.TimeoutHandler's re-panic on the serving goroutine,
	// whose stack has the middleware chain and this test but not the handler.
	stack, _ := panicLine["stack"].(string)
	if !strings.Contains(stack, "panickingHandler") {
		t.Errorf("stack does not contain the panicking handler frame:\n%s", stack)
	}
}

// panickingHandler is a named function so its symbol can be asserted in the stack trace.
func panickingHandler(http.ResponseWriter, *http.Request) {
	panic("kaboom-7f3a")
}

func TestTracingSpanNamesAndProbeFilter(t *testing.T) {
	t.Parallel()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	logger := slog.New(slog.DiscardHandler)
	cfg := config.Config{ServiceName: "api-test", RequestTimeout: 2 * time.Second}
	r := &Readiness{}
	r.Set(true)
	h := NewRouterWithReadinessAndTracing(logger, cfg, r, otelhttp.WithTracerProvider(tp))

	for _, path := range []string{"/api/v1/ping", "/healthz", "/readyz", "/nope"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, http.NoBody))
	}

	spans := exp.GetSpans()
	names := make([]string, 0, len(spans))
	for _, s := range spans {
		names = append(names, s.Name)
	}
	slices.Sort(names)
	// /healthz and /readyz are filtered; the unmatched route has no pattern -> just the method.
	want := []string{"GET", "GET /api/v1/ping"}
	if !slices.Equal(names, want) {
		t.Fatalf("span names = %v, want %v", names, want)
	}

	// The matched span carries http.route even though TimeoutHandler copied the request.
	for _, s := range spans {
		if s.Name != "GET /api/v1/ping" {
			continue
		}
		if !slices.Contains(s.Attributes, semconv.HTTPRoute("/api/v1/ping")) {
			t.Errorf("span attrs missing http.route: %v", s.Attributes)
		}
	}
}

func TestTimeoutMiddleware(t *testing.T) {
	t.Parallel()
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}), Timeout(20*time.Millisecond))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
