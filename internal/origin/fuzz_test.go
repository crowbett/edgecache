package origin

import (
	"net/url"
	"strings"
	"testing"
)

// FuzzJoinPath: for any base and request path the upstream path never leaves
// the base prefix, never contains a dot segment, always starts with "/", and
// the escaped form re-parses to the same path.
func FuzzJoinPath(f *testing.F) {
	for _, c := range [][2]string{
		{"http://h", "/x"},
		{"http://h/base", "/../x"},
		{"http://h/base/", "/a/../../x"},
		{"http://h/base", "/a/.."},
		{"http://h/base", "/%2e%2e/x"},
		{"http://h/p%20q", "/..%2F%2e%2e/a%20b"},
		{"http://h", "/.."},
		{"http://h/base", "/a//b"},
		{"http://h/base", "/a%2Fb"},
		{"http://h/base", ""},
		{"http://h/base", "x"},
		{"http://h/base", "/a/./b/"},
		{"http://h/base", "/./"},
		{"http://h/base", "/..."},
		{"http://h/base", "/%2E/x"},
		{"http://h/b%2Fase", "/x"},
		{"http://h/base", "/a/../.."},
		{"http://h/base", "/;/../x"},
	} {
		f.Add(c[0], c[1])
	}
	f.Fuzz(func(t *testing.T, base, req string) {
		b, err := url.Parse(base)
		if err != nil || b.Host == "" || strings.Contains(b.Path, "/.") {
			return // a base with dot segments is rejected by config, not here
		}
		r, err := url.Parse("http://edge" + req)
		if err != nil || r.Host != "edge" {
			return
		}
		u := *b
		u.Path, u.RawPath = joinPath(b, r)
		if !strings.HasPrefix(u.Path, "/") {
			t.Fatalf("path %q not rooted", u.Path)
		}
		prefix := strings.TrimSuffix(b.Path, "/")
		if !strings.HasPrefix(u.Path, prefix+"/") && u.Path != prefix {
			t.Fatalf("path %q escaped base %q", u.Path, b.Path)
		}
		for seg := range strings.SplitSeq(u.Path, "/") {
			if seg == "." || seg == ".." {
				t.Fatalf("dot segment survived in %q", u.Path)
			}
		}
		esc := u.EscapedPath()
		if !strings.HasPrefix(esc, "/") {
			t.Fatalf("escaped path %q not rooted", esc)
		}
		back, err := url.Parse("http://h" + esc)
		if err != nil {
			t.Fatalf("escaped path %q does not re-parse: %v", esc, err)
		}
		if back.Path != u.Path {
			t.Fatalf("escaped path %q decodes to %q, want %q", esc, back.Path, u.Path)
		}
		if len(u.Path) > len(b.Path)+len(r.Path)+2 {
			t.Fatalf("joined path grew: %d", len(u.Path))
		}
	})
}
