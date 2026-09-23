package cache

import (
	"context"
	"net/http"
	"testing"
)

func TestPurgeRelativePrefix(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *Config) { c.PurgeVerbose = true })
	h.o.set(func(r *http.Request) (reply, error) {
		return rep(200, map[string]string{"Cache-Control": "max-age=60"}, r.URL.Path), nil
	})
	urls := []string{
		"http://a.example/static/app.js",
		"https://b.example:8443/static/site.css",
		"http://a.example/~user/page",
		"http://a.example/api/static/x",
		"http://a.example/staticfile",
	}
	for _, u := range urls {
		expect(t, h.do(http.MethodGet, u), 200, "MISS", "")
	}
	ctx := context.Background()
	tests := []struct {
		prefix string
		want   int
	}{
		{"/static/", 2},  // every host
		{"/%7Euser/", 1}, // escape-normalised like the stored path (~ is unreserved)
		{"/nothing/", 0},
		{"/", 2}, // what is left: /api/static/x and /staticfile
	}
	for _, tc := range tests {
		res, err := h.c.Purge(ctx, PurgeRequest{Prefix: tc.prefix})
		if err != nil || res.Purged != tc.want {
			t.Errorf("Purge(prefix %q) = %+v, %v; want %d purged", tc.prefix, res, err, tc.want)
		}
	}
	if h.st.len() != 0 {
		t.Errorf("%d entries left", h.st.len())
	}
	if h.rec.purged[PurgeKindPrefix] != 5 {
		t.Errorf("Purged(prefix) = %d", h.rec.purged[PurgeKindPrefix])
	}
}

func TestCanonicalPathPrefix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"/static/", "/static/"},
		{"/%7Euser/", "/~user/"},
		{"/a b", "/a%20b"},
		{"/x?b=1&a=2", "/x?b=1&a=2"}, // query: verbatim
		{"/x?", "/x?"},
	} {
		if got := canonicalPathPrefix(tc.in); got != tc.want {
			t.Errorf("canonicalPathPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct {
		in   string
		want bool
	}{{"/a", true}, {"/", true}, {"//host/a", false}, {"http://h/a", false}, {"a/", false}} {
		if got := isPathPrefix(tc.in); got != tc.want {
			t.Errorf("isPathPrefix(%q) = %v", tc.in, got)
		}
	}
}
