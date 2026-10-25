package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func at(ts ...int64) []Request {
	r := make([]Request, 0, len(ts))
	for _, t := range ts {
		r = append(r, Request{At: t})
	}
	return r
}

func ptr(v int64) *int64 { return &v }

type want struct {
	xcache, reason, age, body string
	upstream                  []string
}

func TestTimelineOutcomes(t *testing.T) {
	t.Parallel()
	get, cond, none := []string{"GET → 200"}, []string{"conditional GET → 304"}, []string{}
	cases := []struct {
		name string
		p    TimelineParams
		want []want
	}{
		{"max-age with ETag revalidates", TimelineParams{Origin: Origin{CacheControl: "max-age=60", ETag: `"v1"`}, Requests: at(0, 30, 61, 90)}, []want{
			{"MISS", "", "0", "version 1", get},
			{"HIT", "", "30", "version 1", none},
			{"REVALIDATED", "", "0", "version 1", cond},
			{"HIT", "", "29", "version 1", none},
		}},
		{"max-age without validator refetches", TimelineParams{Origin: Origin{CacheControl: "max-age=60"}, Requests: at(0, 61)}, []want{
			{"MISS", "", "0", "version 1", get},
			{"MISS", "", "0", "version 2", get},
		}},
		{"s-maxage wins in a shared cache", TimelineParams{Origin: Origin{CacheControl: "max-age=10, s-maxage=120", ETag: `"v1"`}, Requests: at(0, 119, 121)}, []want{
			{"MISS", "", "0", "version 1", get},
			{"HIT", "", "119", "version 1", none},
			{"REVALIDATED", "", "0", "version 1", cond},
		}},
		{"stale-while-revalidate refreshes in the background", TimelineParams{Origin: Origin{CacheControl: "max-age=30, stale-while-revalidate=60", ETag: `"v1"`}, Requests: at(0, 40, 45, 200)}, []want{
			{"MISS", "", "0", "version 1", get},
			{"STALE", "stale-while-revalidate", "40", "version 1", cond},
			{"HIT", "", "5", "version 1", none},
			{"REVALIDATED", "", "0", "version 1", cond},
		}},
		{"no-cache always revalidates", TimelineParams{Origin: Origin{CacheControl: "no-cache", ETag: `"v1"`}, Requests: at(0, 5)}, []want{
			{"MISS", "", "0", "version 1", get},
			{"REVALIDATED", "", "0", "version 1", cond},
		}},
		{"origin Age shortens freshness", TimelineParams{Origin: Origin{CacheControl: "max-age=60", Age: "50", ETag: `"v1"`}, Requests: at(0, 5, 11)}, []want{
			{"MISS", "", "50", "version 1", get},
			{"HIT", "", "55", "version 1", none},
			{"REVALIDATED", "", "50", "version 1", cond},
		}},
		{"Expires", TimelineParams{Origin: Origin{ExpiresIn: ptr(60)}, Requests: at(0, 30, 61)}, []want{
			{"MISS", "", "0", "version 1", get},
			{"HIT", "", "30", "version 1", none},
			{"MISS", "", "0", "version 2", get},
		}},
		{"heuristic freshness from Last-Modified", TimelineParams{Origin: Origin{LastModifiedAgo: ptr(3600)}, Requests: at(0, 359, 361)}, []want{
			{"MISS", "", "0", "version 1", get},
			{"HIT", "", "359", "version 1", none},
			{"REVALIDATED", "", "0", "version 1", []string{"conditional GET → 304"}},
		}},
		{"Vary keeps one entry per language", TimelineParams{Origin: Origin{CacheControl: "max-age=60", Vary: "Accept-Language"}, Requests: []Request{
			{At: 0, Headers: map[string]string{"Accept-Language": "en"}},
			{At: 5, Headers: map[string]string{"Accept-Language": "fr"}},
			{At: 10, Headers: map[string]string{"Accept-Language": "en"}},
		}}, []want{
			{"MISS", "", "0", "version 1", get},
			{"MISS", "", "0", "version 2", get},
			{"HIT", "", "10", "version 1", none},
		}},
		{"request no-store bypasses", TimelineParams{Origin: Origin{CacheControl: "max-age=60"}, Requests: []Request{
			{At: 0}, {At: 5, Headers: map[string]string{"Cache-Control": "no-store"}},
		}}, []want{
			{"MISS", "", "0", "version 1", get},
			{"BYPASS", "request-no-store", "", "version 2", get},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r, err := Timeline(c.p)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Steps) != len(c.want) {
				t.Fatalf("got %d steps, want %d", len(r.Steps), len(c.want))
			}
			for i, w := range c.want {
				s := r.Steps[i]
				got := want{s.XCache, s.Reason, s.Age, s.Body, s.Upstream}
				if s.Status != 200 || !reflect.DeepEqual(got, w) {
					t.Errorf("t=%d: got %d %+v, want 200 %+v", s.At, s.Status, got, w)
				}
			}
		})
	}
}

func TestTimelineClientConditionals(t *testing.T) {
	t.Parallel()
	h := func(k, v string) map[string]string { return map[string]string{k: v} }
	cases := []struct {
		name   string
		p      TimelineParams
		status int
		want   want
	}{
		{
			"weak If-None-Match on a hit",
			TimelineParams{Origin: Origin{CacheControl: "max-age=60", ETag: `"v1"`}, Requests: []Request{{At: 0}, {At: 5, Headers: h("If-None-Match", `W/"v1"`)}}},
			304,
			want{"HIT", "", "5", "", []string{}},
		},
		{
			"If-None-Match list on a cold cache",
			TimelineParams{Origin: Origin{CacheControl: "max-age=60", ETag: `"v1"`}, Requests: []Request{{At: 0, Headers: h("If-None-Match", `"x", "v1"`)}}},
			304,
			want{"MISS", "conditional", "0", "", []string{"conditional GET → 304"}},
		},
		{
			"old If-Modified-Since gets the body",
			TimelineParams{Origin: Origin{CacheControl: "max-age=60", LastModifiedAgo: ptr(3600)}, Requests: []Request{{At: 0, Headers: h("If-Modified-Since", "Thu, 01 Jan 1970 00:00:00 GMT")}}},
			200,
			want{"MISS", "", "0", "version 1", []string{"conditional GET → 200"}},
		},
		{
			"only-if-cached with nothing stored",
			TimelineParams{Origin: Origin{CacheControl: "max-age=60"}, Requests: []Request{{At: 0, Headers: h("Cache-Control", "only-if-cached")}}},
			504,
			want{"MISS", "only-if-cached", "", "{\"error\":\"gateway timeout\"}\n", []string{}},
		},
	}
	for _, c := range cases {
		r, err := Timeline(c.p)
		if err != nil {
			t.Fatal(c.name, err)
		}
		s := r.Steps[len(r.Steps)-1]
		got := want{s.XCache, s.Reason, s.Age, s.Body, s.Upstream}
		if s.Status != c.status || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %d %+v, want %d %+v", c.name, s.Status, got, c.status, c.want)
		}
	}
}

func TestTimelineLimits(t *testing.T) {
	t.Parallel()
	reqs := make([]int64, maxRequests)
	reqs[maxRequests-1] = maxSeconds
	if _, err := Timeline(TimelineParams{Requests: at(reqs...)}); err != nil {
		t.Errorf("%d requests up to %d s: %v", maxRequests, maxSeconds, err)
	}
	if _, err := Timeline(TimelineParams{Requests: at(append(reqs, 0)...)}); err == nil {
		t.Errorf("%d requests accepted", maxRequests+1)
	}
}

func TestTimelineSortsAndIsDeterministic(t *testing.T) {
	t.Parallel()
	p := TimelineParams{Origin: Origin{CacheControl: "max-age=30, stale-while-revalidate=60", ETag: `"v1"`}, Requests: at(45, 0, 40)}
	a, err := Timeline(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Timeline(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Error("the same params gave different timelines")
	}
	if a.Steps[0].At != 0 || a.Steps[1].At != 40 || a.Steps[2].At != 45 {
		t.Errorf("steps not in time order: %+v", a.Steps)
	}
}

func TestHandleErrors(t *testing.T) {
	t.Parallel()
	cases := []struct{ method, params, want string }{
		{"nope", `{}`, `unknown method "nope"`},
		{"timeline", `{`, "params:"},
		{"timeline", `{"requests":[]}`, "want 1..40 requests"},
		{"timeline", `{"requests":[{"at":-1}]}`, "request times must be"},
		{"timeline", `{"requests":[{"at":604801}]}`, "request times must be"},
		{"timeline", `{"requests":[{"at":0,"headers":{"Range":"bytes=0-1,3-4"}}]}`, `request header "Range" is not supported`},
		{"timeline", `{"requests":[{"at":0,"headers":{"accept-language":"` + strings.Repeat("x", 513) + `"}}]}`, "over 512 bytes"},
		{"timeline", `{"origin":{"vary":"` + strings.Repeat("x", 513) + `"},"requests":[{"at":0}]}`, "origin Vary is over"},
		{"timeline", `{"origin":{"etag":"v1"},"requests":[{"at":0}]}`, "quoted entity-tag"},
		{"timeline", `{"origin":{"expiresIn":9223372036854775807},"requests":[{"at":0}]}`, "origin expiresIn must be"},
		{"timeline", `{"origin":{"lastModifiedAgo":-604801},"requests":[{"at":0}]}`, "origin lastModifiedAgo must be"},
	}
	for _, c := range cases {
		got, ok := Handle(c.method, []byte(c.params)).(map[string]string)
		if !ok || !strings.Contains(got["error"], c.want) {
			t.Errorf("%s %s: got %v, want an error containing %q", c.method, c.params, got, c.want)
		}
	}
}

func TestTimelineResultHasNoErrorKey(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(Handle("timeline", []byte(`{"origin":{"cacheControl":"max-age=60"},"requests":[{"at":0}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"error"`) || !strings.Contains(string(raw), `"steps"`) {
		t.Errorf("unexpected result %s", raw)
	}
}
