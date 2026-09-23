package rfc9111

import (
	"net/http"
	"testing"
)

func TestHeadMatches(t *testing.T) {
	t.Parallel()
	const lm = "Sat, 19 Sep 2026 12:00:00 GMT"
	stored := http.Header{"Etag": {`"v1"`}, "Last-Modified": {lm}}
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	tests := []struct {
		name string
		head http.Header
		want bool
	}{
		{"etag match", h("ETag", `"v1"`), true},
		{"etag + lm + length match", h("ETag", `"v1"`, "Last-Modified", lm, "Content-Length", "5"), true},
		{"lm in another date form", h("Last-Modified", "Saturday, 19-Sep-26 12:00:00 GMT"), true},
		{"etag differs", h("ETag", `"v2"`), false},
		{"lm differs", h("ETag", `"v1"`, "Last-Modified", "Sat, 19 Sep 2026 12:00:01 GMT"), false},
		{"length differs", h("ETag", `"v1"`, "Content-Length", "6"), false},
		{"length garbage", h("ETag", `"v1"`, "Content-Length", "five"), false},
		{"no validator", h("Content-Length", "5"), false},
		{"weak vs strong is a different value", h("ETag", `W/"v1"`), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := HeadMatches(stored, 5, tc.head); got != tc.want {
				t.Errorf("HeadMatches(%v) = %v, want %v", tc.head, got, tc.want)
			}
		})
	}
}
