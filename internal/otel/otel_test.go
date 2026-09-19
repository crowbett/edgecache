package otel

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestSetupDisabledIsNoop(t *testing.T) {
	t.Parallel()
	shutdown, err := Setup(context.Background(), Config{Enabled: false})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestLogHandlerAddsTraceIDs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(NewLogHandler(slog.NewJSONHandler(&buf, nil)).WithAttrs([]slog.Attr{slog.String("service", "t")}))

	// No span in context -> no ids.
	logger.InfoContext(context.Background(), "plain")
	var plain map[string]any
	if err := json.Unmarshal(buf.Bytes(), &plain); err != nil {
		t.Fatal(err)
	}
	if _, ok := plain[TraceIDKey]; ok {
		t.Errorf("unexpected %s without a span: %v", TraceIDKey, plain)
	}
	buf.Reset()

	// Recording span -> ids present and matching.
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	logger.InfoContext(ctx, "in span")
	span.End()

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got[TraceIDKey] != span.SpanContext().TraceID().String() {
		t.Errorf("%s = %v, want %s", TraceIDKey, got[TraceIDKey], span.SpanContext().TraceID())
	}
	if got[SpanIDKey] != span.SpanContext().SpanID().String() {
		t.Errorf("%s = %v, want %s", SpanIDKey, got[SpanIDKey], span.SpanContext().SpanID())
	}
	if got["service"] != "t" {
		t.Errorf("WithAttrs lost: %v", got)
	}
}

func TestEndpointHelpers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in           string
		wantURL      string
		wantInsecure bool
	}{
		{"localhost:4317", "http://localhost:4317", true},
		{"http://otel-collector:4317", "http://otel-collector:4317", true},
		{"https://otlp.example.com:4317", "https://otlp.example.com:4317", false},
	}
	for _, tt := range tests {
		if got := normalizeEndpoint(tt.in); got != tt.wantURL {
			t.Errorf("normalizeEndpoint(%q) = %q, want %q", tt.in, got, tt.wantURL)
		}
		if got := insecure(tt.in); got != tt.wantInsecure {
			t.Errorf("insecure(%q) = %v, want %v", tt.in, got, tt.wantInsecure)
		}
	}
}
