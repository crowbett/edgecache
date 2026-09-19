package cachekey

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
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
		{"query reserved escape uppercased", "http://h/a?k=%2f%3D", "http://h/a?k=%2F%3D"},
		{"query only empties dropped", "http://h/a?&&", "http://h/a"},
		{"query kept when path empty", "http://h?x=1", "http://h/?x=1"},
		{"ipv6 default port dropped", "http://[::1]:80/a", "http://[::1]/a"},
		{"ipv6 other port kept", "http://[::1]:8080/a", "http://[::1]:8080/a"},
		{"ipv6 no port", "http://[::1]/a", "http://[::1]/a"},
		{"upper hex host lowercased", "http://[2001:DB8::1]/a", "http://[2001:db8::1]/a"},
		{"encoded slash kept distinct", "http://h/a%2Fb", "http://h/a%2Fb"},
		{"encoded space kept", "http://h/a%20b", "http://h/a%20b"},
		{"literal space re-escaped", "http://h/a b", "http://h/a%20b"},
		{"non-ascii path escaped", "http://h/café", "http://h/caf%C3%A9"},
		{"mixed escapes in one segment", "http://h/%41%2f%7e%2A", "http://h/A%2F~%2A"},
		{"query bad escape kept", "http://h/a?k=%zz&b=%4", "http://h/a?b=%4&k=%zz"},
		{"dot segments kept as sent", "http://h/a/../b", "http://h/a/../b"},
		{"userinfo dropped", "http://user:pw@h/a", "http://h/a"},
		{"scheme-less host-less", "/a?b=1", "/a?b=1"},
		{"duplicate pairs kept", "http://h/?a=1&a=1", "http://h/?a=1&a=1"},
		{"bare key sorted", "http://h/?z&a=1&m", "http://h/?a=1&m&z"},
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

func TestCanonicalLongQuery(t *testing.T) {
	t.Parallel()
	// More pairs than the stack span array and more bytes than the scratch buffer:
	// the slow paths must give the same answer as a naive sort.
	var pairs []string
	for i := 99; i >= 0; i-- {
		pairs = append(pairs, fmt.Sprintf("k%03d=%s", i, strings.Repeat("v", 12)))
	}
	u := mustURL(t, "http://h/p?"+strings.Join(pairs, "&"))
	want := append([]string(nil), pairs...)
	sort.Strings(want)
	if got := Canonical(u); got != "http://h/p?"+strings.Join(want, "&") {
		t.Errorf("long query not sorted: %s", got)
	}
	if Primary(http.MethodGet, u) != Primary(http.MethodGet, mustURL(t, "http://h/p?"+strings.Join(want, "&"))) {
		t.Error("long query: sorted and unsorted forms must share a key")
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
	// Names the alloc-free canonicaliser cannot handle fall back to Header.Values.
	long := strings.Repeat("x", 70)
	odd := []string{long, "x y"}
	hOdd := http.Header{http.CanonicalHeaderKey(long): {"1"}, "x y": {"2"}}
	if Secondary(p, odd, hOdd) == Secondary(p, odd, http.Header{}) {
		t.Error("long / non-token vary names must still be looked up")
	}
	hA := http.Header{"Accept": {"a"}}
	hB := hA.Clone()
	if Secondary(p, []string{"accept"}, hA) != Secondary(p, []string{"accept"}, hB) {
		t.Error("Secondary must be deterministic")
	}
	if Secondary(p, []string{"accept"}, http.Header{"Accept": {"a"}}) == Secondary(p, []string{"accept-x"}, http.Header{"Accept-X": {"a"}}) {
		t.Error("vary name must be part of the key")
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
		{"star inside list", []string{"Accept, *"}, "", ErrVaryStar},
		{"dupes across values", []string{"Accept-Encoding", "accept-encoding"}, "accept-encoding", nil},
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

func TestNoAllocs(t *testing.T) {
	// Not parallel: AllocsPerRun forbids it.
	u := mustURL(t, "http://127.0.0.1:8081/obj/123?x=1&a=2")
	h := http.Header{"Accept-Encoding": {"gzip, br"}, "Accept-Language": {"en-US"}}
	vary := []string{"accept-encoding", "accept-language"}
	var k Key
	if n := testing.AllocsPerRun(100, func() { k = Primary(http.MethodGet, u) }); n != 0 {
		t.Errorf("Primary allocs/op = %v, want 0", n)
	}
	if n := testing.AllocsPerRun(100, func() { _ = Secondary(k, vary, h) }); n != 0 {
		t.Errorf("Secondary allocs/op = %v, want 0", n)
	}
	esc := mustURL(t, "http://h/%7Euser/a%2Fb?q=%7e")
	if n := testing.AllocsPerRun(100, func() { _ = Primary(http.MethodGet, esc) }); n != 0 {
		t.Errorf("Primary (escaped path) allocs/op = %v, want 0", n)
	}
}

func BenchmarkPrimary(b *testing.B) {
	u, _ := url.Parse("http://127.0.0.1:8081/obj/123?x=1&a=2")
	b.ReportAllocs()
	for b.Loop() {
		_ = Primary(http.MethodGet, u)
	}
}

func BenchmarkSecondary(b *testing.B) {
	p := Primary(http.MethodGet, &url.URL{Scheme: "http", Host: "h", Path: "/x"})
	vary := []string{"accept-encoding", "accept-language"}
	h := http.Header{"Accept-Encoding": {"gzip, br"}, "Accept-Language": {"en-US"}}
	b.ReportAllocs()
	for b.Loop() {
		_ = Secondary(p, vary, h)
	}
}

func BenchmarkCanonical(b *testing.B) {
	u, _ := url.Parse("http://EXAMPLE.com:80/obj/%7E123?x=1&a=2")
	b.ReportAllocs()
	for b.Loop() {
		_ = Canonical(u)
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
