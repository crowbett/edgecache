package rfc9111

import (
	"net/http"
	"testing"
)

func TestStripHopByHop(t *testing.T) {
	t.Parallel()
	in := http.Header{
		"Connection":        {"close, X-Custom , ,"},
		"Keep-Alive":        {"timeout=5"},
		"Transfer-Encoding": {"chunked"},
		"Te":                {"trailers"},
		"Upgrade":           {"h2c"},
		"Proxy-Connection":  {"keep-alive"},
		"X-Custom":          {"drop-me"},
		"Content-Type":      {"text/plain"},
		"Cache-Control":     {"max-age=60", "public"},
	}
	got := StripHopByHop(in)
	for _, k := range []string{"Connection", "Keep-Alive", "Transfer-Encoding", "Te", "Upgrade", "Proxy-Connection", "X-Custom"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s should have been stripped", k)
		}
	}
	if got.Get("Content-Type") != "text/plain" || len(got.Values("Cache-Control")) != 2 {
		t.Errorf("end-to-end fields altered: %v", got)
	}
	if in.Get("X-Custom") != "drop-me" || in.Get("Connection") == "" {
		t.Error("input must not be modified")
	}
	got["Cache-Control"][0] = "mutated"
	if in["Cache-Control"][0] != "max-age=60" {
		t.Error("output must not alias the input's slices")
	}
	if n := StripHopByHop(nil); n == nil || len(n) != 0 {
		t.Errorf("nil input -> empty header, got %v", n)
	}
}

func TestActionString(t *testing.T) {
	t.Parallel()
	want := map[Action]string{
		Fresh: "fresh", Stale: "stale", Revalidate: "revalidate",
		StaleWhileRevalidate: "stale-while-revalidate", Bypass: "bypass", GatewayTimeout: "gateway-timeout",
		Action(99): "unknown",
	}
	for a, s := range want {
		if a.String() != s {
			t.Errorf("Action(%d).String() = %q, want %q", int(a), a.String(), s)
		}
	}
}
