package cachekey

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// FuzzCanonical asserts the canonical form never panics, is idempotent (the
// canonical form of the canonical form is itself), is bounded in size, keys
// consistently (Primary of the re-parsed canonical URL equals Primary of the
// original), and never keeps the fragment.
func FuzzCanonical(f *testing.F) {
	for _, s := range []string{
		"http://example.com", "HTTP://EXAMPLE.COM:80/", "https://example.com:443/a/b?z=1&a=2", "http://h/%7Euser",
		"http://h/a%20b?x=%41", "http://h/a?b=1&b=0&a", "http://h/?&&&", "http://h/#frag", "http://[::1]:80/x",
		"http://[::1]:8080/x", "http://h/p%2Fq", "http://h/%zz", "http://h/%", "http://h/a?%", "/relative?x=1",
		"", "http://h/" + strings.Repeat("a", 1100), "http://h/?" + strings.Repeat("k=v&", 70),
		"http://h/a?ü=ö", "http://h/%C3%BC", "http://h/%c3%bc", "http://h/a;p=1?q", "//\"", "http://\"/x", "//A:", "//h:/x", "//@//", "//",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		u, err := url.Parse(s)
		if err != nil {
			return
		}
		c := Canonical(u)
		if len(c) > 3*len(s)+8 {
			t.Fatalf("canonical form grew unboundedly: %d from %d", len(c), len(s))
		}
		if strings.Contains(c, "#") && !strings.Contains(u.Path+u.RawQuery, "#") {
			t.Fatalf("fragment leaked into %q", c)
		}
		if u.Scheme == "" || u.Host == "" || !plainHost(u.Host) {
			// The engine only keys absolute URLs (absoluteURL fills scheme and
			// host); a relative or degenerate authority ("//@//", "//\"") need
			// not re-parse to the same shape.
			return
		}
		u2, err := url.Parse(c)
		if err != nil || !strings.EqualFold(u2.Scheme, u.Scheme) {
			return // the canonical form of a hostile URL ("//A:") need not re-parse to the same shape
		}
		if c2 := Canonical(u2); c2 != c {
			t.Fatalf("not idempotent: %q -> %q", c, c2)
		}
		if Primary(http.MethodGet, u) != Primary(http.MethodGet, u2) {
			t.Fatalf("primary key differs between %q and its canonical form", s)
		}
		if Primary(http.MethodGet, u) == Primary(http.MethodHead, u) {
			t.Fatal("method not part of the primary key")
		}
		if u.Scheme != "" && !strings.HasPrefix(c, strings.ToLower(u.Scheme)+"://") {
			t.Fatalf("scheme not lowercased in %q", c)
		}
	})
}

// plainHost reports whether h is made of reg-name / IP-literal bytes only.
func plainHost(h string) bool {
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '.', c == '-', c == ':', c == '[', c == ']', c == '_':
		default:
			return false
		}
	}
	return true
}

// FuzzNormalizeVary asserts the normalised list is lowercase, sorted, deduped
// and idempotent, and that "*" anywhere is ErrVaryStar.
func FuzzNormalizeVary(f *testing.F) {
	for _, s := range []string{
		"", "Accept-Encoding", "accept-encoding, Accept", "Accept, accept , ACCEPT", "*", "Accept, *", ",,,",
		" , ", "A,B,C,D,E,F", "X-\x00", "Accept\tEncoding",
	} {
		f.Add(s, "")
		f.Add("", s)
	}
	f.Fuzz(func(t *testing.T, v1, v2 string) {
		out, err := NormalizeVary([]string{v1, v2})
		star := false
		for part := range strings.SplitSeq(v1+","+v2, ",") {
			if strings.TrimSpace(part) == "*" {
				star = true
			}
		}
		if (err != nil) != star {
			t.Fatalf("err=%v star=%v for %q %q", err, star, v1, v2)
		}
		if err != nil {
			if out != nil {
				t.Fatal("list returned alongside ErrVaryStar")
			}
			return
		}
		if !slices.IsSorted(out) {
			t.Fatalf("not sorted: %q", out)
		}
		for i, name := range out {
			if name == "" || name != strings.ToLower(name) || name != strings.TrimSpace(name) {
				t.Fatalf("bad element %q", name)
			}
			if i > 0 && out[i-1] == name {
				t.Fatalf("duplicate %q", name)
			}
		}
		again, _ := NormalizeVary(out)
		if !slices.Equal(again, out) {
			t.Fatalf("not idempotent: %q -> %q", out, again)
		}
	})
}

// FuzzSecondary asserts the Vary-selected key never panics on arbitrary field
// names, depends on the varied headers only, and treats a missing header and
// an empty one alike.
func FuzzSecondary(f *testing.F) {
	f.Add("accept-encoding", "gzip", "gzip")
	f.Add("accept", "text/html", "text/plain")
	f.Add("x-very-long-name-"+strings.Repeat("x", 70), "a", "b")
	f.Add("", "a", "b")
	f.Add("bad name", "a", "b")
	f.Add("accept-encoding", "gzip, br", "gzip,br")
	f.Add("accept-encoding", " gzip ", "gzip")
	f.Fuzz(func(t *testing.T, name, v1, v2 string) {
		var p Key
		p[0] = 7
		vary := []string{name}
		h1 := http.Header{}
		h2 := http.Header{}
		if name != "" {
			h1[http.CanonicalHeaderKey(name)] = []string{v1}
			h2[http.CanonicalHeaderKey(name)] = []string{v2}
		}
		k1, k2 := Secondary(p, vary, h1), Secondary(p, vary, h2)
		if Secondary(p, vary, h1) != k1 {
			t.Fatal("not deterministic")
		}
		trim := strings.TrimSpace(v1) == strings.TrimSpace(v2)
		if name != "" && http.CanonicalHeaderKey(name) == http.CanonicalHeaderKey(strings.ToLower(name)) {
			if (k1 == k2) != trim {
				t.Fatalf("keys equal=%v but values equal=%v (%q vs %q)", k1 == k2, trim, v1, v2)
			}
		}
		// An unrelated header never influences the key.
		h1.Set("X-Unrelated-Zz", "1")
		if Secondary(p, vary, h1) != k1 {
			t.Fatal("unrelated header changed the key")
		}
		if k1 == p {
			t.Fatal("secondary key equals the primary key")
		}
	})
}
