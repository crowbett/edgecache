package cachekey

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return u
}

func TestCanonical(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, want string
	}{
		{"lowercase host", "http://EXAMPLE.com/a", "http://example.com/a"},
		{"lowercase scheme", "HTTP://example.com/a", "http://example.com/a"},
		{"default http port dropped", "http://example.com:80/a", "http://example.com/a"},
		{"default https port dropped", "https://example.com:443/a", "https://example.com/a"},
		{"non-default port kept", "http://example.com:8080/a", "http://example.com:8080/a"},
		{"https on 80 kept", "https://example.com:80/a", "https://example.com:80/a"},
		{"empty path", "http://example.com", "http://example.com/"},
		{"unreserved decoded", "http://example.com/%7Euser/%41", "http://example.com/~user/A"},
		{"reserved escape uppercased", "http://example.com/a%2fb%3f", "http://example.com/a%2Fb%3F"},
		{"query sorted", "http://example.com/?b=2&a=1&a=0", "http://example.com/?a=0&a=1&b=2"},
		{"empty query dropped", "http://example.com/a?", "http://example.com/a"},
		{"empty pairs dropped", "http://example.com/a?&x=1&&", "http://example.com/a?x=1"},
		{"fragment dropped", "http://example.com/a#frag", "http://example.com/a"},
		{"path case kept", "http://example.com/A/B", "http://example.com/A/B"},
		{"relative request target", "/obj/1?z=1&y=2", "/obj/1?y=2&z=1"},
		{"query escape normalised", "http://h/a?k=%7e", "http://h/a?k=~"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Canonical(mustURL(t, tt.in)); got != tt.want {
				t.Errorf("Canonical(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPrimary(t *testing.T) {
	t.Parallel()
	a := Primary(http.MethodGet, mustURL(t, "http://EXAMPLE.com:80/x?b=1&a=2"))
	b := Primary(http.MethodGet, mustURL(t, "http://example.com/x?a=2&b=1"))
	if a != b {
		t.Errorf("equivalent URLs differ: %s vs %s", a, b)
	}
	if c := Primary(http.MethodHead, mustURL(t, "http://example.com/x?a=2&b=1")); c == a {
		t.Error("method must be part of the key")
	}
	if d := Primary(http.MethodGet, mustURL(t, "http://example.com/x?a=2&b=2")); d == a {
		t.Error("query value must be part of the key")
	}
	if len(a.String()) != 64 || strings.ToLower(a.String()) != a.String() {
		t.Errorf("String() = %q, want 64 lowercase hex chars", a.String())
	}
}

func TestSecondary(t *testing.T) {
	t.Parallel()
	p := Primary(http.MethodGet, mustURL(t, "http://example.com/x"))
	vary := []string{"accept-encoding", "accept-language"}
	h1 := http.Header{"Accept-Encoding": {"gzip"}, "Accept-Language": {"en"}}
	h2 := http.Header{"Accept-Encoding": {" gzip "}, "Accept-Language": {"en"}}
	h3 := http.Header{"Accept-Encoding": {"br"}, "Accept-Language": {"en"}}
	h4 := http.Header{"Accept-Language": {"en"}}
	if Secondary(p, vary, h1) != Secondary(p, vary, h2) {
		t.Error("whitespace around a value must not change the key")
	}
	if Secondary(p, vary, h1) == Secondary(p, vary, h3) {
		t.Error("different header value must change the key")
	}
	if Secondary(p, vary, h1) == Secondary(p, vary, h4) {
		t.Error("missing header must change the key")
	}
	if Secondary(p, nil, h1) == p {
		t.Error("no-vary secondary key must still differ from the primary")
	}
	if Secondary(p, nil, h1) != Secondary(p, nil, h3) {
		t.Error("no-vary secondary key must ignore headers")
	}
	multi := http.Header{"Accept-Encoding": {"gzip", "br"}, "Accept-Language": {"en"}}
	joined := http.Header{"Accept-Encoding": {"gzip, br"}, "Accept-Language": {"en"}}
	if Secondary(p, vary, multi) != Secondary(p, vary, joined) {
		t.Error("repeated header must hash like its comma-joined form")
	}
}

func TestNormalizeVary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []string
		want string
		err  error
	}{
		{"single", []string{"Accept-Encoding"}, "accept-encoding", nil},
		{"list in one value", []string{"Accept-Encoding, Accept-Language"}, "accept-encoding,accept-language", nil},
		{"multiple values sorted deduped", []string{"b", "A, b", " a "}, "a,b", nil},
		{"empty", nil, "", nil},
		{"blanks only", []string{" , "}, "", nil},
		{"star", []string{"Accept", "*"}, "", ErrVaryStar},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeVary(tt.in)
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if s := strings.Join(got, ","); s != tt.want {
				t.Errorf("NormalizeVary(%q) = %q, want %q", tt.in, s, tt.want)
			}
		})
	}
}

func BenchmarkKeyBuild(b *testing.B) {
	u, _ := url.Parse("http://127.0.0.1:8081/obj/123?x=1&a=2")
	vary := []string{"accept-encoding", "accept-language"}
	h := http.Header{"Accept-Encoding": {"gzip, br"}, "Accept-Language": {"en-US"}}
	b.ReportAllocs()
	for b.Loop() {
		p := Primary(http.MethodGet, u)
		_ = Secondary(p, vary, h)
	}
}
