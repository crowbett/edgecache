package rfc9111

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// t0 is the fixed clock every test runs on; nothing here calls time.Now.
var t0 = time.Date(2026, time.September, 19, 12, 0, 0, 0, time.UTC)

func httpDate(t time.Time) string { return t.UTC().Format(http.TimeFormat) }

// hdr builds a Header from "Name: value" lines; repeated names add values.
func hdr(lines ...string) http.Header {
	h := http.Header{}
	for _, l := range lines {
		name, value, _ := strings.Cut(l, ":")
		h.Add(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	return h
}

// resp is a stored exchange received at t0 with Date = t0 unless a Date line is given.
func resp(status int, lifetime time.Duration, lines ...string) Resp {
	h := hdr(lines...)
	if h.Get("Date") == "" {
		h.Set("Date", httpDate(t0))
	}
	return Resp{Status: status, Header: h, RequestTime: t0, ResponseTime: t0, Lifetime: lifetime}
}

func getReq(lines ...string) *http.Request {
	return &http.Request{Method: http.MethodGet, Header: hdr(lines...)}
}

// ---------------------------------------------------------------------------
// Parsers

func TestParseRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		h    http.Header
		want ReqDirectives
	}{
		{"empty", hdr(), ReqDirectives{}},
		{"no-cache token", hdr("Cache-Control: no-cache"), ReqDirectives{NoCache: true}},
		{"case-insensitive", hdr("Cache-Control: NO-STORE, Only-If-Cached"), ReqDirectives{NoStore: true, OnlyIfCached: true}},
		{"max-age", hdr("Cache-Control: max-age=30"), ReqDirectives{MaxAge: 30 * time.Second, HasMaxAge: true}},
		{"max-age quoted", hdr(`Cache-Control: max-age="30"`), ReqDirectives{MaxAge: 30 * time.Second, HasMaxAge: true}},
		{"max-age=0", hdr("Cache-Control: max-age=0"), ReqDirectives{HasMaxAge: true}},
		{"max-age invalid counts as 0", hdr("Cache-Control: max-age=abc"), ReqDirectives{HasMaxAge: true}},
		{"max-age duplicate keeps first", hdr("Cache-Control: max-age=10, max-age=20"), ReqDirectives{MaxAge: 10 * time.Second, HasMaxAge: true}},
		{"max-stale bare", hdr("Cache-Control: max-stale"), ReqDirectives{HasMaxStale: true, MaxStaleAny: true}},
		{"max-stale valued", hdr("Cache-Control: max-stale=60"), ReqDirectives{HasMaxStale: true, MaxStale: time.Minute}},
		{"max-stale invalid ignored", hdr("Cache-Control: max-stale=x"), ReqDirectives{}},
		{"min-fresh", hdr("Cache-Control: min-fresh=5"), ReqDirectives{HasMinFresh: true, MinFresh: 5 * time.Second}},
		{"min-fresh invalid ignored", hdr("Cache-Control: min-fresh="), ReqDirectives{}},
		{"no-transform", hdr("Cache-Control: no-transform"), ReqDirectives{NoTransform: true}},
		{"stale-if-error", hdr("Cache-Control: stale-if-error=120"), ReqDirectives{HasStaleIfError: true, StaleIfError: 2 * time.Minute}},
		{"multiple lines", hdr("Cache-Control: no-cache", "Cache-Control: max-age=5"), ReqDirectives{NoCache: true, MaxAge: 5 * time.Second, HasMaxAge: true}},
		{"whitespace and empty members", hdr("Cache-Control:  , no-cache ,, max-age = 7 ,"), ReqDirectives{NoCache: true, MaxAge: 7 * time.Second, HasMaxAge: true}},
		{"extension ignored", hdr("Cache-Control: community=\"UCI\", no-cache"), ReqDirectives{NoCache: true}},
		{"pragma fallback", hdr("Pragma: no-cache"), ReqDirectives{NoCache: true}},
		{"pragma ignored with cache-control", hdr("Pragma: no-cache", "Cache-Control: max-age=5"), ReqDirectives{MaxAge: 5 * time.Second, HasMaxAge: true}},
		{"pragma other tokens", hdr("Pragma: foo, No-Cache"), ReqDirectives{NoCache: true}},
		{"overflow capped", hdr("Cache-Control: max-age=99999999999999"), ReqDirectives{MaxAge: MaxDelta, HasMaxAge: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ParseRequest(tc.h); got != tc.want {
				t.Errorf("ParseRequest(%v)\n got %+v\nwant %+v", tc.h, got, tc.want)
			}
		})
	}
}

func TestParseResponse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		h    http.Header
		want RespDirectives
	}{
		{"empty", hdr(), RespDirectives{}},
		{"public max-age", hdr("Cache-Control: public, max-age=3600"), RespDirectives{Public: true, MaxAge: time.Hour, HasMaxAge: true}},
		{"s-maxage and max-age", hdr("Cache-Control: s-maxage=10, max-age=60"), RespDirectives{SMaxAge: 10 * time.Second, HasSMaxAge: true, MaxAge: time.Minute, HasMaxAge: true}},
		{"no-store", hdr("Cache-Control: no-store"), RespDirectives{NoStore: true}},
		{"no-cache unqualified", hdr("Cache-Control: no-cache"), RespDirectives{NoCache: true}},
		{"no-cache qualified", hdr(`Cache-Control: no-cache="Set-Cookie"`), RespDirectives{NoCacheFields: []string{"Set-Cookie"}}},
		{"no-cache qualified list", hdr(`Cache-Control: no-cache="set-cookie, x-foo"`), RespDirectives{NoCacheFields: []string{"Set-Cookie", "X-Foo"}}},
		{"no-cache empty list is unqualified", hdr(`Cache-Control: no-cache=""`), RespDirectives{NoCache: true}},
		{"private qualified", hdr(`Cache-Control: private="X-Token", max-age=5`), RespDirectives{PrivateFields: []string{"X-Token"}, MaxAge: 5 * time.Second, HasMaxAge: true}},
		{"private unqualified", hdr("Cache-Control: private"), RespDirectives{Private: true}},
		{"private token form", hdr("Cache-Control: private=Set-Cookie"), RespDirectives{PrivateFields: []string{"Set-Cookie"}}},
		{"must-revalidate proxy-revalidate immutable", hdr("Cache-Control: must-revalidate, proxy-revalidate, immutable"), RespDirectives{MustRevalidate: true, ProxyRevalidate: true, Immutable: true}},
		{"rfc5861", hdr("Cache-Control: max-age=600, stale-while-revalidate=30, stale-if-error=1200"), RespDirectives{MaxAge: 10 * time.Minute, HasMaxAge: true, StaleWhileRevalidate: 30 * time.Second, HasSWR: true, StaleIfError: 20 * time.Minute, HasSIE: true}},
		{"swr invalid ignored", hdr("Cache-Control: stale-while-revalidate=abc, stale-if-error"), RespDirectives{}},
		{"max-age invalid is 0", hdr("Cache-Control: max-age=1.5"), RespDirectives{HasMaxAge: true}},
		{"max-age negative is 0", hdr("Cache-Control: max-age=-1"), RespDirectives{HasMaxAge: true}},
		{"max-age overflow capped", hdr("Cache-Control: max-age=2147483648"), RespDirectives{MaxAge: MaxDelta, HasMaxAge: true}},
		{"max-age at cap", hdr("Cache-Control: max-age=2147483647"), RespDirectives{MaxAge: 2147483647 * time.Second, HasMaxAge: true}},
		{"duplicate keeps first", hdr("Cache-Control: max-age=1", "Cache-Control: max-age=2, s-maxage=3, s-maxage=4"), RespDirectives{MaxAge: time.Second, HasMaxAge: true, SMaxAge: 3 * time.Second, HasSMaxAge: true}},
		{"quoted-pair", hdr(`Cache-Control: no-cache="X-\"Q\"", public`), RespDirectives{NoCacheFields: []string{`X-"Q"`}, Public: true}},
		{"unterminated quote", hdr(`Cache-Control: no-cache="Set-Cookie, public`), RespDirectives{NoCacheFields: []string{"Set-Cookie", "Public"}}},
		{"junk after quote", hdr(`Cache-Control: max-age="7"xyz, public`), RespDirectives{MaxAge: 7 * time.Second, HasMaxAge: true, Public: true}},
		{"spaces around equals", hdr("Cache-Control: max-age = 9"), RespDirectives{MaxAge: 9 * time.Second, HasMaxAge: true}},
		{"case", hdr("Cache-Control: Public, S-MAXAGE=2"), RespDirectives{Public: true, SMaxAge: 2 * time.Second, HasSMaxAge: true}},
		{"bare equals", hdr("Cache-Control: =, max-age"), RespDirectives{HasMaxAge: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ParseResponse(tc.h); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseResponse(%v)\n got %+v\nwant %+v", tc.h, got, tc.want)
			}
		})
	}
}

func TestParseDate(t *testing.T) {
	t.Parallel()
	want := time.Date(1994, time.November, 6, 8, 49, 37, 0, time.UTC)
	tests := []struct {
		name string
		in   string
		ref  time.Time
		want time.Time
		ok   bool
	}{
		{"imf-fixdate", "Sun, 06 Nov 1994 08:49:37 GMT", t0, want, true},
		{"imf-fixdate UTC tolerated", "Sun, 06 Nov 1994 08:49:37 UTC", t0, want, true},
		{"imf-fixdate surrounding OWS", "  Sun, 06 Nov 1994 08:49:37 GMT ", t0, want, true},
		{"rfc850", "Sunday, 06-Nov-94 08:49:37 GMT", t0, want, true},
		{"rfc850 two-digit year > 50y ahead moves back", "Saturday, 19-Sep-60 12:00:00 GMT", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1960, time.September, 19, 12, 0, 0, 0, time.UTC), true},
		{"rfc850 zero ref keeps go's pivot", "Saturday, 19-Sep-60 12:00:00 GMT", time.Time{}, time.Date(2060, time.September, 19, 12, 0, 0, 0, time.UTC), true},
		{"asctime", "Sun Nov  6 08:49:37 1994", t0, want, true},
		{"asctime two-digit day", "Sat Sep 19 12:00:00 2026", t0, t0, true},
		{"31 feb", "Tue, 31 Feb 2026 00:00:00 GMT", t0, time.Time{}, false},
		{"day 00", "Tue, 00 Feb 2026 00:00:00 GMT", t0, time.Time{}, false},
		{"bad month", "Sun, 06 Foo 1994 08:49:37 GMT", t0, time.Time{}, false},
		{"hour 24", "Sun, 06 Nov 1994 24:00:00 GMT", t0, time.Time{}, false},
		{"zone EST", "Sun, 06 Nov 1994 08:49:37 EST", t0, time.Time{}, false},
		{"rfc3339", "1994-11-06T08:49:37Z", t0, time.Time{}, false},
		{"zero", "0", t0, time.Time{}, false},
		{"negative", "-1", t0, time.Time{}, false},
		{"empty", "", t0, time.Time{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ParseDate(tc.in, tc.ref)
			if ok != tc.ok || !got.Equal(tc.want) {
				t.Errorf("ParseDate(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
			}
			if ok && got.Location() != time.UTC {
				t.Errorf("ParseDate(%q) location = %v, want UTC", tc.in, got.Location())
			}
		})
	}
}

func TestParseDateIMFNoAlloc(t *testing.T) {
	s := httpDate(t0)
	allocs := testing.AllocsPerRun(200, func() {
		if _, ok := ParseDate(s, t0); !ok {
			t.Fatal("parse failed")
		}
	})
	if allocs != 0 {
		t.Errorf("IMF-fixdate fast path allocates: %v allocs/op", allocs)
	}
}

// ---------------------------------------------------------------------------
// Storable (§3)

func TestStorable(t *testing.T) {
	t.Parallel()
	type tc struct {
		name   string
		policy Policy
		req    *http.Request
		res    Resp
		ok     bool
		reason Reason
	}
	tests := []tc{
		{"200 max-age", Policy{}, getReq(), resp(200, 0, "Cache-Control: max-age=60"), true, ReasonNone},
		{"HEAD", Policy{}, &http.Request{Method: http.MethodHead, Header: hdr()}, resp(200, 0, "Cache-Control: max-age=60"), true, ReasonNone},
		{"nil request", Policy{}, nil, resp(200, 0, "Cache-Control: max-age=60"), true, ReasonNone},
		{"POST", Policy{}, &http.Request{Method: http.MethodPost, Header: hdr()}, resp(200, 0, "Cache-Control: max-age=60"), false, ReasonUnsafeMethod},
		{"DELETE", Policy{}, &http.Request{Method: http.MethodDelete, Header: hdr()}, resp(200, 0, "Cache-Control: max-age=60"), false, ReasonUnsafeMethod},
		{"OPTIONS", Policy{}, &http.Request{Method: http.MethodOptions, Header: hdr()}, resp(200, 0, "Cache-Control: max-age=60"), false, ReasonMethod},
		{"request no-store", Policy{}, getReq("Cache-Control: no-store"), resp(200, 0, "Cache-Control: max-age=60"), false, ReasonReqNoStore},
		{"response no-store", Policy{}, getReq(), resp(200, 0, "Cache-Control: no-store, max-age=60"), false, ReasonRespNoStore},
		{"private", Policy{}, getReq(), resp(200, 0, "Cache-Control: private, max-age=60"), false, ReasonPrivate},
		{"private qualified is storable", Policy{}, getReq(), resp(200, 0, `Cache-Control: private="Set-Cookie", max-age=60`), true, ReasonNone},
		{"no-cache qualified is storable", Policy{}, getReq(), resp(200, 0, `Cache-Control: no-cache="Set-Cookie", max-age=60`), true, ReasonNone},
		{"304", Policy{}, getReq(), resp(304, 0, "Cache-Control: max-age=60"), false, ReasonStatus},
		{"1xx", Policy{}, getReq(), resp(101, 0, "Cache-Control: max-age=60"), false, ReasonStatus},
		{"206", Policy{}, getReq(), resp(206, 0, "Cache-Control: max-age=60", "ETag: \"a\""), false, ReasonRange},
		{"content-range on 200", Policy{}, getReq(), resp(200, 0, "Cache-Control: max-age=60", "Content-Range: bytes 0-1/2"), false, ReasonRange},
		{"range request", Policy{}, getReq("Range: bytes=0-1"), resp(200, 0, "Cache-Control: max-age=60"), false, ReasonRange},
		{"vary star", Policy{}, getReq(), resp(200, 0, "Cache-Control: max-age=60", "Vary: *"), false, ReasonVaryStar},
		{"vary list with star", Policy{}, getReq(), resp(200, 0, "Cache-Control: max-age=60", "Vary: Accept-Encoding", "Vary: Accept, *"), false, ReasonVaryStar},
		{"vary normal", Policy{}, getReq(), resp(200, 0, "Cache-Control: max-age=60", "Vary: Accept-Encoding"), true, ReasonNone},
		{"authorization", Policy{}, getReq("Authorization: Bearer x"), resp(200, 0, "Cache-Control: max-age=60"), false, ReasonAuthorization},
		{"authorization + public", Policy{}, getReq("Authorization: Bearer x"), resp(200, 0, "Cache-Control: public, max-age=60"), true, ReasonNone},
		{"authorization + s-maxage", Policy{}, getReq("Authorization: Bearer x"), resp(200, 0, "Cache-Control: s-maxage=60"), true, ReasonNone},
		{"authorization + must-revalidate", Policy{}, getReq("Authorization: Bearer x"), resp(200, 0, "Cache-Control: max-age=60, must-revalidate"), true, ReasonNone},
		{"authorization overridden by policy", Policy{CacheAuthorization: true}, getReq("Authorization: Bearer x"), resp(200, 0, "Cache-Control: max-age=60"), true, ReasonNone},
		{"302 without freshness", Policy{}, getReq(), resp(302, 0, "Last-Modified: "+httpDate(t0.Add(-time.Hour))), false, ReasonStatus},
		{"302 with max-age", Policy{}, getReq(), resp(302, 0, "Cache-Control: max-age=60"), true, ReasonNone},
		{"302 with Expires", Policy{}, getReq(), resp(302, 0, "Expires: "+httpDate(t0.Add(time.Hour))), true, ReasonNone},
		{"302 public only", Policy{}, getReq(), resp(302, 0, "Cache-Control: public", "ETag: \"a\""), true, ReasonNone},
		{"500 with max-age", Policy{}, getReq(), resp(500, 0, "Cache-Control: max-age=5"), true, ReasonNone},
		{"500 without freshness", Policy{}, getReq(), resp(500, 0), false, ReasonStatus},
		{"404 heuristic", Policy{}, getReq(), resp(404, 0, "Last-Modified: "+httpDate(t0.Add(-24*time.Hour))), true, ReasonNone},
		{"501 heuristic", Policy{}, getReq(), resp(501, 0, "Last-Modified: "+httpDate(t0.Add(-24*time.Hour))), true, ReasonNone},
		{"200 no freshness no validator", Policy{}, getReq(), resp(200, 0), false, ReasonNoFreshness},
		{"200 no freshness with ETag", Policy{}, getReq(), resp(200, 0, "ETag: \"a\""), true, ReasonNone},
		{"200 Last-Modified only", Policy{}, getReq(), resp(200, 0, "Last-Modified: "+httpDate(t0.Add(-24*time.Hour))), true, ReasonNone},
		{"no-cache with validator", Policy{}, getReq(), resp(200, 0, "Cache-Control: no-cache", "ETag: \"a\""), true, ReasonNone},
		{"no-cache without validator", Policy{}, getReq(), resp(200, 0, "Cache-Control: no-cache, max-age=60"), false, ReasonRespNoCache},
		{"max-age=0 with ETag", Policy{}, getReq(), resp(200, 0, "Cache-Control: max-age=0", "ETag: \"a\""), true, ReasonNone},
		{"max-age=0 without validator", Policy{}, getReq(), resp(200, 0, "Cache-Control: max-age=0"), false, ReasonNoFreshness},
		{"max-age=0 with swr", Policy{}, getReq(), resp(200, 0, "Cache-Control: max-age=0, stale-while-revalidate=30"), true, ReasonNone},
		{"max-age=0 with default sie", Policy{DefaultSIE: time.Minute}, getReq(), resp(200, 0, "Cache-Control: max-age=0"), true, ReasonNone},
		{"Expires 0 without validator", Policy{}, getReq(), resp(200, 0, "Expires: 0"), false, ReasonNoFreshness},
		{"Expires 0 with validator", Policy{}, getReq(), resp(200, 0, "Expires: 0", "ETag: \"a\""), true, ReasonNone},
		{"no-store beats method", Policy{}, &http.Request{Method: http.MethodPost, Header: hdr()}, resp(200, 0, "Cache-Control: no-store"), false, ReasonUnsafeMethod},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var rd ReqDirectives
			if tc.req != nil {
				rd = ParseRequest(tc.req.Header)
			}
			sd := ParseResponse(tc.res.Header)
			ok, reason := tc.policy.Storable(tc.req, rd, tc.res, sd)
			if ok != tc.ok || reason != tc.reason {
				t.Errorf("Storable = %v, %q; want %v, %q", ok, reason, tc.ok, tc.reason)
			}
		})
	}
}

func TestHeuristicallyCacheable(t *testing.T) {
	t.Parallel()
	yes := []int{200, 203, 204, 300, 301, 308, 404, 405, 410, 414, 451, 501}
	no := []int{201, 202, 206, 302, 303, 304, 307, 400, 401, 403, 500, 502, 503}
	for _, s := range yes {
		if !HeuristicallyCacheable(s) {
			t.Errorf("%d should be heuristically cacheable", s)
		}
	}
	for _, s := range no {
		if HeuristicallyCacheable(s) {
			t.Errorf("%d should not be heuristically cacheable", s)
		}
	}
}

// ---------------------------------------------------------------------------
// Lifetime (§4.2.1, §4.2.2)

func TestLifetime(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	tests := []struct {
		name      string
		policy    Policy
		res       Resp
		want      time.Duration
		heuristic bool
	}{
		{"s-maxage beats max-age beats Expires", Policy{}, resp(200, 0, "Cache-Control: s-maxage=10, max-age=60", "Expires: "+httpDate(t0.Add(time.Hour))), 10 * time.Second, false},
		{"max-age beats Expires", Policy{}, resp(200, 0, "Cache-Control: max-age=60", "Expires: "+httpDate(t0.Add(time.Hour))), time.Minute, false},
		{"max-age beats Last-Modified", Policy{}, resp(200, 0, "Cache-Control: max-age=60", "Last-Modified: "+httpDate(t0.Add(-10*day))), time.Minute, false},
		{"s-maxage=0", Policy{}, resp(200, 0, "Cache-Control: s-maxage=0, max-age=60"), 0, false},
		{"max-age invalid is 0", Policy{}, resp(200, 0, "Cache-Control: max-age=abc", "Expires: "+httpDate(t0.Add(time.Hour))), 0, false},
		{"Expires minus Date", Policy{}, resp(200, 0, "Expires: "+httpDate(t0.Add(5*time.Minute))), 5 * time.Minute, false},
		{"Expires rfc850", Policy{}, resp(200, 0, "Expires: Saturday, 19-Sep-26 12:05:00 GMT"), 5 * time.Minute, false},
		{"Expires asctime", Policy{}, resp(200, 0, "Expires: Sat Sep 19 12:05:00 2026"), 5 * time.Minute, false},
		{"Expires equal Date", Policy{}, resp(200, 0, "Expires: "+httpDate(t0)), 0, false},
		{"Expires before Date clamps", Policy{}, resp(200, 0, "Expires: "+httpDate(t0.Add(-time.Hour))), 0, false},
		{"Expires 0", Policy{}, resp(200, 0, "Expires: 0", "Last-Modified: "+httpDate(t0.Add(-10*day))), 0, false},
		{"Expires -1", Policy{}, resp(200, 0, "Expires: -1"), 0, false},
		{"Expires garbage suppresses heuristic", Policy{}, resp(200, 0, "Expires: soon", "Last-Modified: "+httpDate(t0.Add(-10*day))), 0, false},
		{"Expires first value wins", Policy{}, resp(200, 0, "Expires: "+httpDate(t0.Add(time.Minute)), "Expires: "+httpDate(t0.Add(time.Hour))), time.Minute, false},
		{"Expires with no Date uses response time", Policy{}, Resp{Status: 200, Header: hdr("Expires: " + httpDate(t0.Add(90*time.Second))), RequestTime: t0, ResponseTime: t0}, 90 * time.Second, false},
		{"Expires with invalid Date uses response time", Policy{}, resp(200, 0, "Date: yesterday", "Expires: "+httpDate(t0.Add(90*time.Second))), 90 * time.Second, false},
		{"Expires against origin Date not receipt", Policy{}, Resp{Status: 200, Header: hdr("Date: "+httpDate(t0.Add(-time.Hour)), "Expires: "+httpDate(t0)), RequestTime: t0, ResponseTime: t0}, time.Hour, false},
		{"heuristic 10 percent", Policy{}, resp(200, 0, "Last-Modified: "+httpDate(t0.Add(-5*day))), 12 * time.Hour, true},
		{"heuristic capped 24h", Policy{}, resp(200, 0, "Last-Modified: "+httpDate(t0.Add(-30*day))), day, true},
		{"heuristic exactly at cap", Policy{}, resp(200, 0, "Last-Modified: "+httpDate(t0.Add(-10*day))), day, true},
		{"heuristic custom policy", Policy{HeuristicFraction: 0.5, HeuristicMax: time.Hour}, resp(200, 0, "Last-Modified: "+httpDate(t0.Add(-5*day))), time.Hour, true},
		{"heuristic custom fraction", Policy{HeuristicFraction: 0.5, HeuristicMax: 10 * day}, resp(200, 0, "Last-Modified: "+httpDate(t0.Add(-day))), 12 * time.Hour, true},
		{"heuristic 404", Policy{}, resp(404, 0, "Last-Modified: "+httpDate(t0.Add(-day))), 144 * time.Minute, true},
		{"heuristic not for 302", Policy{}, resp(302, 0, "Last-Modified: "+httpDate(t0.Add(-day))), 0, false},
		{"heuristic needs Last-Modified", Policy{}, resp(200, 0, "ETag: \"a\""), 0, false},
		{"heuristic Last-Modified after Date", Policy{}, resp(200, 0, "Last-Modified: "+httpDate(t0.Add(time.Hour))), 0, false},
		{"heuristic Last-Modified equals Date", Policy{}, resp(200, 0, "Last-Modified: "+httpDate(t0)), 0, false},
		{"heuristic invalid Last-Modified", Policy{}, resp(200, 0, "Last-Modified: recently"), 0, false},
		{"max-age overflow", Policy{}, resp(200, 0, "Cache-Control: max-age=99999999999"), MaxDelta, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, heuristic := tc.policy.Lifetime(tc.res, ParseResponse(tc.res.Header))
			if got != tc.want || heuristic != tc.heuristic {
				t.Errorf("Lifetime = %v, %v; want %v, %v", got, heuristic, tc.want, tc.heuristic)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Age (§4.2.3)

func TestAge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		res  Resp
		now  time.Time
		want time.Duration
	}{
		{
			"age header plus response delay",
			Resp{Header: hdr("Date: "+httpDate(t0), "Age: 10"), RequestTime: t0, ResponseTime: t0.Add(2 * time.Second)},
			t0.Add(32 * time.Second),
			42 * time.Second, // corrected_age_value 10+2 > apparent 2; + 30 resident
		},
		{
			"apparent age wins over age header",
			Resp{Header: hdr("Date: "+httpDate(t0.Add(-100*time.Second)), "Age: 10"), RequestTime: t0, ResponseTime: t0.Add(2 * time.Second)},
			t0.Add(32 * time.Second),
			132 * time.Second, // apparent 102 > 12; + 30
		},
		{
			"no age header",
			Resp{Header: hdr("Date: " + httpDate(t0)), RequestTime: t0, ResponseTime: t0},
			t0.Add(time.Minute),
			time.Minute,
		},
		{
			"date in the future clamps apparent age",
			Resp{Header: hdr("Date: " + httpDate(t0.Add(time.Hour))), RequestTime: t0, ResponseTime: t0.Add(time.Second)},
			t0.Add(11 * time.Second),
			11 * time.Second, // delay 1 + resident 10
		},
		{
			"missing date uses response time",
			Resp{Header: hdr("Age: 5"), RequestTime: t0, ResponseTime: t0},
			t0.Add(5 * time.Second),
			10 * time.Second,
		},
		{
			"invalid date uses response time",
			Resp{Header: hdr("Date: tomorrow"), RequestTime: t0, ResponseTime: t0},
			t0.Add(5 * time.Second),
			5 * time.Second,
		},
		{
			"invalid age header is 0",
			Resp{Header: hdr("Date: "+httpDate(t0), "Age: abc"), RequestTime: t0, ResponseTime: t0},
			t0.Add(5 * time.Second),
			5 * time.Second,
		},
		{
			"age list takes first member",
			Resp{Header: hdr("Date: "+httpDate(t0), "Age: 7, 100"), RequestTime: t0, ResponseTime: t0},
			t0,
			7 * time.Second,
		},
		{
			"age overflow capped",
			Resp{Header: hdr("Date: "+httpDate(t0), "Age: 99999999999"), RequestTime: t0, ResponseTime: t0},
			t0,
			MaxDelta,
		},
		{
			"now before response time clamps at 0",
			Resp{Header: hdr("Date: " + httpDate(t0)), RequestTime: t0, ResponseTime: t0},
			t0.Add(-time.Minute),
			0,
		},
		{
			"zero times use only the age header",
			Resp{Header: hdr("Age: 3")},
			t0,
			3 * time.Second,
		},
		{
			"request after response contributes 0 delay",
			Resp{Header: hdr("Date: " + httpDate(t0)), RequestTime: t0.Add(time.Hour), ResponseTime: t0},
			t0.Add(time.Second),
			time.Second,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Age(tc.res, tc.now); got != tc.want {
				t.Errorf("Age = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Evaluate (§4, §5.2.1, §5.2.2, RFC 5861)

func TestEvaluate(t *testing.T) {
	t.Parallel()
	const lifetime = time.Minute
	etag := "ETag: \"v1\""
	tests := []struct {
		name   string
		policy Policy
		req    http.Header
		res    Resp
		age    time.Duration
		want   Decision
	}{
		{"fresh", Policy{}, hdr(), resp(200, lifetime, etag), 30 * time.Second, Decision{Action: Fresh, Reason: ReasonFresh}},
		{"fresh at age 0", Policy{}, hdr(), resp(200, lifetime, etag), 0, Decision{Action: Fresh, Reason: ReasonFresh}},
		{"stale at age == lifetime", Policy{}, hdr(), resp(200, lifetime, etag), lifetime, Decision{Action: Revalidate, Reason: ReasonExpired}},
		{"expired with validator", Policy{}, hdr(), resp(200, lifetime, etag), 90 * time.Second, Decision{Action: Revalidate, Reason: ReasonExpired}},
		{"expired with Last-Modified", Policy{}, hdr(), resp(200, lifetime, "Last-Modified: "+httpDate(t0.Add(-time.Hour))), 90 * time.Second, Decision{Action: Revalidate, Reason: ReasonExpired}},
		{"expired without validator", Policy{}, hdr(), resp(200, lifetime), 90 * time.Second, Decision{Action: Stale, Reason: ReasonExpired}},
		{"heuristic expired", Policy{}, hdr(), heuristicResp(lifetime, etag), 90 * time.Second, Decision{Action: Revalidate, Reason: ReasonHeuristic}},
		{"heuristic fresh", Policy{}, hdr(), heuristicResp(lifetime, etag), 30 * time.Second, Decision{Action: Fresh, Reason: ReasonFresh}},
		{"zero lifetime is stale", Policy{}, hdr(), resp(200, 0, etag), 0, Decision{Action: Revalidate, Reason: ReasonExpired}},

		{"request no-store", Policy{}, hdr("Cache-Control: no-store"), resp(200, lifetime, etag), 30 * time.Second, Decision{Action: Bypass, Reason: ReasonReqNoStore}},
		{"response no-cache", Policy{}, hdr(), resp(200, lifetime, etag, "Cache-Control: no-cache"), 30 * time.Second, Decision{Action: Revalidate, Reason: ReasonRespNoCache}},
		{"response no-cache qualified is fresh", Policy{}, hdr(), resp(200, lifetime, etag, `Cache-Control: no-cache="Set-Cookie", max-age=60`), 30 * time.Second, Decision{Action: Fresh, Reason: ReasonFresh}},
		{"request no-cache", Policy{}, hdr("Cache-Control: no-cache"), resp(200, lifetime, etag), 30 * time.Second, Decision{Action: Revalidate, Reason: ReasonReqNoCache}},
		{"request no-cache without validator", Policy{}, hdr("Cache-Control: no-cache"), resp(200, lifetime), 30 * time.Second, Decision{Action: Stale, Reason: ReasonReqNoCache}},
		{"pragma no-cache", Policy{}, hdr("Pragma: no-cache"), resp(200, lifetime, etag), 30 * time.Second, Decision{Action: Revalidate, Reason: ReasonReqNoCache}},
		{"request no-cache beats immutable", Policy{}, hdr("Cache-Control: no-cache"), resp(200, lifetime, etag, "Cache-Control: max-age=60, immutable"), 30 * time.Second, Decision{Action: Revalidate, Reason: ReasonReqNoCache}},

		{"max-age=0", Policy{}, hdr("Cache-Control: max-age=0"), resp(200, lifetime, etag), 30 * time.Second, Decision{Action: Revalidate, Reason: ReasonMaxAge}},
		{"max-age above age", Policy{}, hdr("Cache-Control: max-age=40"), resp(200, lifetime, etag), 30 * time.Second, Decision{Action: Fresh, Reason: ReasonFresh}},
		{"max-age equal age", Policy{}, hdr("Cache-Control: max-age=30"), resp(200, lifetime, etag), 30 * time.Second, Decision{Action: Fresh, Reason: ReasonFresh}},
		{"max-age below age", Policy{}, hdr("Cache-Control: max-age=10"), resp(200, lifetime, etag), 30 * time.Second, Decision{Action: Revalidate, Reason: ReasonMaxAge}},
		{"max-age below age no validator", Policy{}, hdr("Cache-Control: max-age=10"), resp(200, lifetime), 30 * time.Second, Decision{Action: Stale, Reason: ReasonMaxAge}},
		{"min-fresh satisfied", Policy{}, hdr("Cache-Control: min-fresh=20"), resp(200, lifetime, etag), 30 * time.Second, Decision{Action: Fresh, Reason: ReasonFresh}},
		{"min-fresh exact", Policy{}, hdr("Cache-Control: min-fresh=30"), resp(200, lifetime, etag), 30 * time.Second, Decision{Action: Fresh, Reason: ReasonFresh}},
		{"min-fresh not satisfied", Policy{}, hdr("Cache-Control: min-fresh=40"), resp(200, lifetime, etag), 30 * time.Second, Decision{Action: Revalidate, Reason: ReasonMinFresh}},

		{"max-stale any", Policy{}, hdr("Cache-Control: max-stale"), resp(200, lifetime, etag), 90 * time.Second, Decision{Action: Fresh, Reason: ReasonMaxStale}},
		{"max-stale any no validator", Policy{}, hdr("Cache-Control: max-stale"), resp(200, lifetime), time.Hour, Decision{Action: Fresh, Reason: ReasonMaxStale}},
		{"max-stale within", Policy{}, hdr("Cache-Control: max-stale=30"), resp(200, lifetime, etag), 80 * time.Second, Decision{Action: Fresh, Reason: ReasonMaxStale}},
		{"max-stale exact", Policy{}, hdr("Cache-Control: max-stale=30"), resp(200, lifetime, etag), 90 * time.Second, Decision{Action: Fresh, Reason: ReasonMaxStale}},
		{"max-stale exceeded", Policy{}, hdr("Cache-Control: max-stale=30"), resp(200, lifetime, etag), 100 * time.Second, Decision{Action: Revalidate, Reason: ReasonExpired}},
		{"max-stale on fresh entry", Policy{}, hdr("Cache-Control: max-stale=30"), resp(200, lifetime, etag), 10 * time.Second, Decision{Action: Fresh, Reason: ReasonFresh}},

		{"must-revalidate fresh", Policy{}, hdr(), resp(200, lifetime, etag, "Cache-Control: max-age=60, must-revalidate"), 30 * time.Second, Decision{Action: Fresh, Reason: ReasonFresh}},
		{"must-revalidate forbids max-stale", Policy{}, hdr("Cache-Control: max-stale"), resp(200, lifetime, etag, "Cache-Control: max-age=60, must-revalidate"), 90 * time.Second, Decision{Action: Revalidate, Reason: ReasonMustRevalidate}},
		{"must-revalidate forbids swr", Policy{}, hdr(), resp(200, lifetime, etag, "Cache-Control: max-age=60, must-revalidate, stale-while-revalidate=600"), 90 * time.Second, Decision{Action: Revalidate, Reason: ReasonMustRevalidate}},
		{"proxy-revalidate forbids swr", Policy{}, hdr(), resp(200, lifetime, etag, "Cache-Control: max-age=60, proxy-revalidate, stale-while-revalidate=600"), 90 * time.Second, Decision{Action: Revalidate, Reason: ReasonMustRevalidate}},
		{"must-revalidate without validator", Policy{}, hdr("Cache-Control: max-stale"), resp(200, lifetime, "Cache-Control: max-age=60, must-revalidate"), 90 * time.Second, Decision{Action: Stale, Reason: ReasonMustRevalidate}},
		{"must-revalidate only-if-cached", Policy{}, hdr("Cache-Control: only-if-cached, max-stale"), resp(200, lifetime, etag, "Cache-Control: max-age=60, must-revalidate"), 90 * time.Second, Decision{Action: GatewayTimeout, Reason: ReasonOnlyIfCached}},

		{"swr within window", Policy{}, hdr(), resp(200, lifetime, etag, "Cache-Control: max-age=60, stale-while-revalidate=30"), 80 * time.Second, Decision{Action: StaleWhileRevalidate, Reason: ReasonSWR, Background: true}},
		{"swr exact", Policy{}, hdr(), resp(200, lifetime, etag, "Cache-Control: max-age=60, stale-while-revalidate=30"), 90 * time.Second, Decision{Action: StaleWhileRevalidate, Reason: ReasonSWR, Background: true}},
		{"swr exceeded", Policy{}, hdr(), resp(200, lifetime, etag, "Cache-Control: max-age=60, stale-while-revalidate=30"), 100 * time.Second, Decision{Action: Revalidate, Reason: ReasonExpired}},
		{"swr without validator", Policy{}, hdr(), resp(200, lifetime, "Cache-Control: max-age=60, stale-while-revalidate=30"), 80 * time.Second, Decision{Action: StaleWhileRevalidate, Reason: ReasonSWR, Background: true}},
		{"swr fresh entry", Policy{}, hdr(), resp(200, lifetime, etag, "Cache-Control: max-age=60, stale-while-revalidate=30"), 30 * time.Second, Decision{Action: Fresh, Reason: ReasonFresh}},
		{"default swr applied", Policy{DefaultSWR: 30 * time.Second}, hdr(), resp(200, lifetime, etag, "Cache-Control: max-age=60"), 80 * time.Second, Decision{Action: StaleWhileRevalidate, Reason: ReasonSWR, Background: true}},
		{"default swr not applied past window", Policy{DefaultSWR: 30 * time.Second}, hdr(), resp(200, lifetime, etag, "Cache-Control: max-age=60"), 100 * time.Second, Decision{Action: Revalidate, Reason: ReasonExpired}},
		{"origin swr=0 beats default", Policy{DefaultSWR: 30 * time.Second}, hdr(), resp(200, lifetime, etag, "Cache-Control: max-age=60, stale-while-revalidate=0"), 80 * time.Second, Decision{Action: Revalidate, Reason: ReasonExpired}},
		{"max-stale beats swr", Policy{}, hdr("Cache-Control: max-stale"), resp(200, lifetime, etag, "Cache-Control: max-age=60, stale-while-revalidate=30"), 80 * time.Second, Decision{Action: Fresh, Reason: ReasonMaxStale}},
		{"swr with request max-age", Policy{}, hdr("Cache-Control: max-age=10"), resp(200, lifetime, etag, "Cache-Control: max-age=60, stale-while-revalidate=30"), 80 * time.Second, Decision{Action: Revalidate, Reason: ReasonMaxAge}},

		{"only-if-cached fresh", Policy{}, hdr("Cache-Control: only-if-cached"), resp(200, lifetime, etag), 30 * time.Second, Decision{Action: Fresh, Reason: ReasonFresh}},
		{"only-if-cached stale", Policy{}, hdr("Cache-Control: only-if-cached"), resp(200, lifetime, etag), 90 * time.Second, Decision{Action: GatewayTimeout, Reason: ReasonOnlyIfCached}},
		{"only-if-cached stale no validator", Policy{}, hdr("Cache-Control: only-if-cached"), resp(200, lifetime), 90 * time.Second, Decision{Action: GatewayTimeout, Reason: ReasonOnlyIfCached}},
		{"only-if-cached with max-stale", Policy{}, hdr("Cache-Control: only-if-cached, max-stale"), resp(200, lifetime, etag), 90 * time.Second, Decision{Action: Fresh, Reason: ReasonMaxStale}},
		{"only-if-cached with swr", Policy{}, hdr("Cache-Control: only-if-cached"), resp(200, lifetime, etag, "Cache-Control: max-age=60, stale-while-revalidate=30"), 80 * time.Second, Decision{Action: StaleWhileRevalidate, Reason: ReasonSWR, Background: true}},
		{"only-if-cached with no-cache", Policy{}, hdr("Cache-Control: only-if-cached, no-cache"), resp(200, lifetime, etag), 30 * time.Second, Decision{Action: GatewayTimeout, Reason: ReasonOnlyIfCached}},
		{"only-if-cached response no-cache", Policy{}, hdr("Cache-Control: only-if-cached"), resp(200, lifetime, etag, "Cache-Control: no-cache"), 30 * time.Second, Decision{Action: GatewayTimeout, Reason: ReasonOnlyIfCached}},
		{"no-store beats only-if-cached", Policy{}, hdr("Cache-Control: only-if-cached, no-store"), resp(200, lifetime, etag), 30 * time.Second, Decision{Action: Bypass, Reason: ReasonReqNoStore}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.want.Age = tc.age
			got := tc.policy.Evaluate(t0.Add(tc.age), tc.res, ParseResponse(tc.res.Header), ParseRequest(tc.req))
			if got != tc.want {
				t.Errorf("Evaluate\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// heuristicResp is a stored 200 whose lifetime came from §4.2.2.
func heuristicResp(lifetime time.Duration, lines ...string) Resp {
	r := resp(200, lifetime, append(lines, "Last-Modified: "+httpDate(t0.Add(-10*24*time.Hour)))...)
	r.Heuristic = true
	return r
}

func TestEvaluateAgeIncludesHeader(t *testing.T) {
	t.Parallel()
	// Age: 50 on receipt, lifetime 60, 20 s resident: current age 70 -> stale.
	res := resp(200, time.Minute, "Age: 50", "ETag: \"v1\"")
	got := Policy{}.Evaluate(t0.Add(20*time.Second), res, ParseResponse(res.Header), ReqDirectives{})
	want := Decision{Action: Revalidate, Reason: ReasonExpired, Age: 70 * time.Second}
	if got != want {
		t.Errorf("Evaluate = %+v, want %+v", got, want)
	}
}

func TestStaleIfError(t *testing.T) {
	t.Parallel()
	const lifetime = time.Minute
	tests := []struct {
		name   string
		policy Policy
		req    http.Header
		res    Resp
		age    time.Duration
		ok     bool
		reason Reason
	}{
		{"fresh always ok", Policy{}, hdr(), resp(200, lifetime), 30 * time.Second, true, ReasonFresh},
		{"stale without window", Policy{}, hdr(), resp(200, lifetime), 90 * time.Second, false, ReasonExpired},
		{"response window within", Policy{}, hdr(), resp(200, lifetime, "Cache-Control: max-age=60, stale-if-error=30"), 80 * time.Second, true, ReasonSIE},
		{"response window exact", Policy{}, hdr(), resp(200, lifetime, "Cache-Control: max-age=60, stale-if-error=30"), 90 * time.Second, true, ReasonSIE},
		{"response window exceeded", Policy{}, hdr(), resp(200, lifetime, "Cache-Control: max-age=60, stale-if-error=30"), 100 * time.Second, false, ReasonExpired},
		{"request window widens", Policy{}, hdr("Cache-Control: stale-if-error=100"), resp(200, lifetime, "Cache-Control: max-age=60, stale-if-error=30"), 100 * time.Second, true, ReasonSIE},
		{"request window alone", Policy{}, hdr("Cache-Control: stale-if-error=100"), resp(200, lifetime), 100 * time.Second, true, ReasonSIE},
		{"default window", Policy{DefaultSIE: 50 * time.Second}, hdr(), resp(200, lifetime), 100 * time.Second, true, ReasonSIE},
		{"origin sie=0 beats default", Policy{DefaultSIE: 50 * time.Second}, hdr(), resp(200, lifetime, "Cache-Control: max-age=60, stale-if-error=0"), 100 * time.Second, false, ReasonExpired},
		{"must-revalidate", Policy{}, hdr(), resp(200, lifetime, "Cache-Control: max-age=60, must-revalidate, stale-if-error=600"), 90 * time.Second, false, ReasonMustRevalidate},
		{"proxy-revalidate", Policy{}, hdr(), resp(200, lifetime, "Cache-Control: max-age=60, proxy-revalidate, stale-if-error=600"), 90 * time.Second, false, ReasonMustRevalidate},
		{"must-revalidate even when fresh", Policy{}, hdr(), resp(200, lifetime, "Cache-Control: max-age=60, must-revalidate"), 30 * time.Second, false, ReasonMustRevalidate},
		{"response no-cache", Policy{}, hdr(), resp(200, lifetime, "Cache-Control: no-cache, stale-if-error=600"), 90 * time.Second, false, ReasonRespNoCache},
		{"request no-cache", Policy{}, hdr("Cache-Control: no-cache"), resp(200, lifetime, "Cache-Control: max-age=60, stale-if-error=600"), 90 * time.Second, false, ReasonReqNoCache},
		{"request no-store", Policy{}, hdr("Cache-Control: no-store"), resp(200, lifetime, "Cache-Control: max-age=60, stale-if-error=600"), 30 * time.Second, false, ReasonReqNoStore},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ok, reason := tc.policy.StaleIfError(t0.Add(tc.age), tc.res, ParseResponse(tc.res.Header), ParseRequest(tc.req))
			if ok != tc.ok || reason != tc.reason {
				t.Errorf("StaleIfError = %v, %q; want %v, %q", ok, reason, tc.ok, tc.reason)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Validation (§4.3)

func TestConditionalRequest(t *testing.T) {
	t.Parallel()
	lm := httpDate(t0.Add(-time.Hour))
	tests := []struct {
		name   string
		in     http.Header
		stored http.Header
		want   http.Header
	}{
		{"etag only", hdr(), hdr("ETag: \"v1\""), hdr("If-None-Match: \"v1\"")},
		{"weak etag", hdr(), hdr("ETag: W/\"v1\""), hdr("If-None-Match: W/\"v1\"")},
		{"last-modified only", hdr(), hdr("Last-Modified: " + lm), hdr("If-Modified-Since: " + lm)},
		{"both", hdr(), hdr("ETag: \"v1\"", "Last-Modified: "+lm), hdr("If-None-Match: \"v1\"", "If-Modified-Since: "+lm)},
		{"multiple etags joined", hdr(), hdr("ETag: \"a\"", "ETag: \"b\""), hdr("If-None-Match: \"a\", \"b\"")},
		{"no validator adds nothing", hdr("Accept: */*"), hdr("Content-Type: text/plain"), hdr("Accept: */*")},
		{"replaces client conditionals", hdr("If-None-Match: \"client\"", "If-Modified-Since: "+httpDate(t0)), hdr("ETag: \"v1\"", "Last-Modified: "+lm), hdr("If-None-Match: \"v1\"", "If-Modified-Since: "+lm)},
		{"keeps client conditional when stored lacks that validator", hdr("If-Modified-Since: " + httpDate(t0)), hdr("ETag: \"v1\""), hdr("If-None-Match: \"v1\"", "If-Modified-Since: "+httpDate(t0))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ConditionalRequest(tc.in, Resp{Header: tc.stored})
			if !reflect.DeepEqual(tc.in, tc.want) {
				t.Errorf("ConditionalRequest\n got %v\nwant %v", tc.in, tc.want)
			}
		})
	}
	t.Run("nil header", func(t *testing.T) {
		t.Parallel()
		ConditionalRequest(nil, Resp{Header: hdr("ETag: \"v1\"")}) // must not panic
	})
}

func TestNotModified(t *testing.T) {
	t.Parallel()
	lm := httpDate(t0)
	tests := []struct {
		name   string
		req    http.Header
		stored http.Header
		want   bool
	}{
		{"strong match", hdr("If-None-Match: \"v1\""), hdr("ETag: \"v1\""), true},
		{"weak vs strong matches", hdr("If-None-Match: W/\"v1\""), hdr("ETag: \"v1\""), true},
		{"strong vs weak matches", hdr("If-None-Match: \"v1\""), hdr("ETag: W/\"v1\""), true},
		{"list match", hdr("If-None-Match: \"a\", W/\"b\", \"v1\""), hdr("ETag: \"v1\""), true},
		{"list across lines", hdr("If-None-Match: \"a\"", "If-None-Match: \"v1\""), hdr("ETag: \"v1\""), true},
		{"no match", hdr("If-None-Match: \"v2\""), hdr("ETag: \"v1\""), false},
		{"prefix is not a match", hdr("If-None-Match: \"v\""), hdr("ETag: \"v1\""), false},
		{"star", hdr("If-None-Match: *"), hdr("ETag: \"v1\""), true},
		{"star without stored etag", hdr("If-None-Match: *"), hdr("Last-Modified: " + lm), false},
		{"comma inside tag", hdr("If-None-Match: \"a,b\", \"c\""), hdr("ETag: \"a,b\""), true},
		{"no stored etag", hdr("If-None-Match: \"v1\""), hdr(), false},
		{"inm takes precedence over ims", hdr("If-None-Match: \"v2\"", "If-Modified-Since: "+lm), hdr("ETag: \"v1\"", "Last-Modified: "+lm), false},
		{"ims equal", hdr("If-Modified-Since: " + lm), hdr("Last-Modified: " + lm), true},
		{"ims later", hdr("If-Modified-Since: " + httpDate(t0.Add(time.Hour))), hdr("Last-Modified: " + lm), true},
		{"ims earlier", hdr("If-Modified-Since: " + httpDate(t0.Add(-time.Second))), hdr("Last-Modified: " + lm), false},
		{"ims rfc850", hdr("If-Modified-Since: Saturday, 19-Sep-26 12:00:00 GMT"), hdr("Last-Modified: " + lm), true},
		{"ims invalid", hdr("If-Modified-Since: yesterday"), hdr("Last-Modified: " + lm), false},
		{"ims without stored last-modified", hdr("If-Modified-Since: " + lm), hdr("ETag: \"v1\""), false},
		{"unconditional", hdr(), hdr("ETag: \"v1\"", "Last-Modified: "+lm), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NotModified(tc.req, tc.stored); got != tc.want {
				t.Errorf("NotModified = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMerge304(t *testing.T) {
	t.Parallel()
	stored := hdr(
		"Date: "+httpDate(t0.Add(-time.Hour)),
		"Age: 100",
		"Cache-Control: max-age=60",
		"ETag: \"v1\"",
		"Content-Type: text/html",
		"Content-Length: 1234",
		"Content-Encoding: gzip",
		"X-Custom: a",
		"X-Custom: b",
		"Keep: me",
	)
	fresh := hdr(
		"Date: "+httpDate(t0),
		"Cache-Control: max-age=600",
		"ETag: \"v1\"",
		"X-Custom: c",
		"Content-Length: 0",
		"Content-Encoding: identity",
		"Content-Range: bytes 0-0/1",
		"Connection: close, X-Hop",
		"X-Hop: gone",
		"Transfer-Encoding: chunked",
		"Keep-Alive: timeout=5",
		"Vary: Accept",
	)
	storedCopy, freshCopy := stored.Clone(), fresh.Clone()
	got := Merge304(stored, fresh)
	want := hdr(
		"Date: "+httpDate(t0),
		"Cache-Control: max-age=600",
		"ETag: \"v1\"",
		"Content-Type: text/html",
		"Content-Length: 1234",
		"Content-Encoding: gzip",
		"X-Custom: c",
		"Keep: me",
		"Vary: Accept",
	)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge304\n got %v\nwant %v", got, want)
	}
	if !reflect.DeepEqual(stored, storedCopy) || !reflect.DeepEqual(fresh, freshCopy) {
		t.Error("inputs must not be modified")
	}
	got["Keep"][0] = "mutated"
	if stored.Get("Keep") != "me" {
		t.Error("output must not alias stored slices")
	}

	t.Run("date dropped when 304 has none", func(t *testing.T) {
		t.Parallel()
		got := Merge304(hdr("Date: "+httpDate(t0), "Age: 5", "ETag: \"v1\""), hdr("ETag: \"v1\""))
		if got.Get("Date") != "" || got.Get("Age") != "" || got.Get("ETag") != "\"v1\"" {
			t.Errorf("got %v", got)
		}
	})
	t.Run("empty 304 keeps everything but time fields", func(t *testing.T) {
		t.Parallel()
		got := Merge304(hdr("Content-Type: a", "Expires: "+httpDate(t0)), http.Header{})
		if !reflect.DeepEqual(got, hdr("Content-Type: a", "Expires: "+httpDate(t0))) {
			t.Errorf("got %v", got)
		}
	})
}

func TestStorableHeader(t *testing.T) {
	t.Parallel()
	in := hdr(
		"Cache-Control: no-cache=\"Set-Cookie\", private=\"X-Token, X-Other\", max-age=60",
		"Set-Cookie: a=b",
		"X-Token: t",
		"X-Other: o",
		"Warning: 110 - stale",
		"Connection: X-Hop",
		"X-Hop: 1",
		"Transfer-Encoding: chunked",
		"Content-Type: text/plain",
	)
	inCopy := in.Clone()
	sd := ParseResponse(in)
	got := StorableHeader(in, sd, t0)
	want := hdr(
		"Cache-Control: no-cache=\"Set-Cookie\", private=\"X-Token, X-Other\", max-age=60",
		"Content-Type: text/plain",
		"Date: "+httpDate(t0),
	)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("StorableHeader\n got %v\nwant %v", got, want)
	}
	if !reflect.DeepEqual(in, inCopy) {
		t.Error("input must not be modified")
	}
	if again := StorableHeader(got, sd, t0.Add(time.Hour)); !reflect.DeepEqual(again, got) {
		t.Errorf("not idempotent: %v", again)
	}
	t.Run("keeps origin Date", func(t *testing.T) {
		t.Parallel()
		d := httpDate(t0.Add(-time.Hour))
		if got := StorableHeader(hdr("Date: "+d), RespDirectives{}, t0); got.Get("Date") != d {
			t.Errorf("Date = %q, want %q", got.Get("Date"), d)
		}
	})
	t.Run("zero received adds no Date", func(t *testing.T) {
		t.Parallel()
		if got := StorableHeader(hdr("X: 1"), RespDirectives{}, time.Time{}); got.Get("Date") != "" {
			t.Errorf("unexpected Date %q", got.Get("Date"))
		}
	})
}

func TestHasValidator(t *testing.T) {
	t.Parallel()
	if HasValidator(hdr()) || !HasValidator(hdr("ETag: \"a\"")) || !HasValidator(hdr("Last-Modified: "+httpDate(t0))) {
		t.Error("HasValidator mismatch")
	}
}

// ---------------------------------------------------------------------------
// End-to-end: the engine's call order on one exchange, with a fixed clock.

func TestStoreThenServe(t *testing.T) {
	t.Parallel()
	p := Policy{}
	req := getReq("Accept: text/html")
	rd := ParseRequest(req.Header)
	origin := hdr(
		"Date: "+httpDate(t0),
		"Cache-Control: max-age=60, stale-while-revalidate=30, no-cache=\"Set-Cookie\"",
		"Set-Cookie: session=1",
		"ETag: \"v1\"",
		"Connection: close",
		"Content-Type: text/html",
	)
	sd := ParseResponse(origin)
	res := Resp{Status: 200, Header: origin, RequestTime: t0.Add(-time.Second), ResponseTime: t0}
	if ok, reason := p.Storable(req, rd, res, sd); !ok {
		t.Fatalf("not storable: %s", reason)
	}
	res.Lifetime, res.Heuristic = p.Lifetime(res, sd)
	res.Header = StorableHeader(res.Header, sd, res.ResponseTime)
	if res.Lifetime != time.Minute || res.Heuristic || res.Header.Get("Set-Cookie") != "" || res.Header.Get("Connection") != "" {
		t.Fatalf("stored form wrong: %+v", res)
	}

	if d := p.Evaluate(t0.Add(30*time.Second), res, sd, rd); d.Action != Fresh || d.Age != 31*time.Second {
		t.Errorf("t+30s: %+v", d)
	}
	if d := p.Evaluate(t0.Add(75*time.Second), res, sd, rd); d.Action != StaleWhileRevalidate || !d.Background {
		t.Errorf("t+75s: %+v", d)
	}
	d := p.Evaluate(t0.Add(120*time.Second), res, sd, rd)
	if d.Action != Revalidate {
		t.Fatalf("t+120s: %+v", d)
	}
	cond := hdr("Accept: text/html")
	ConditionalRequest(cond, res)
	if cond.Get("If-None-Match") != "\"v1\"" {
		t.Fatalf("conditional: %v", cond)
	}
	now := t0.Add(121 * time.Second)
	notModified := hdr("Date: "+httpDate(now), "Cache-Control: max-age=600", "ETag: \"v1\"", "Connection: close")
	sd2 := ParseResponse(notModified)
	res2 := Resp{Status: res.Status, RequestTime: t0.Add(120 * time.Second), ResponseTime: now}
	res2.Header = StorableHeader(Merge304(res.Header, notModified), sd2, now)
	res2.Lifetime, res2.Heuristic = p.Lifetime(res2, sd2)
	if res2.Lifetime != 10*time.Minute || res2.Header.Get("Content-Type") != "text/html" || res2.Header.Get("Connection") != "" {
		t.Fatalf("merged form wrong: %+v", res2)
	}
	if d := p.Evaluate(now.Add(5*time.Minute), res2, sd2, rd); d.Action != Fresh || d.Age != 5*time.Minute+time.Second {
		t.Errorf("after revalidation: %+v", d)
	}
}

// ---------------------------------------------------------------------------
// Fuzz + benchmarks

func FuzzParseCacheControl(f *testing.F) {
	for _, s := range []string{
		"", ",", "=", "\"", "no-cache", "max-age=60, public",
		`no-cache="Set-Cookie", private="X-A, X-B"`, `max-age="60"`,
		"max-stale, min-fresh=5, only-if-cached", "max-age=99999999999999999999",
		`no-cache="a\"b\\c`, "s-maxage=1,,,max-age=2", "stale-while-revalidate=30, stale-if-error=60",
		"max-age = 1 , max-age=2", "Pragma", "\x00\xff", "no-cache=\"" + strings.Repeat("a,", 100),
	} {
		f.Add(s, s)
	}
	f.Fuzz(func(t *testing.T, cc, pragma string) {
		h := http.Header{"Cache-Control": {cc, pragma}, "Pragma": {pragma}}
		rd := ParseRequest(h)
		sd := ParseResponse(h)
		if rd != ParseRequest(h) || !reflect.DeepEqual(sd, ParseResponse(h)) {
			t.Fatal("parse is not deterministic")
		}
		if sd.MaxAge > MaxDelta || sd.SMaxAge > MaxDelta || rd.MaxAge > MaxDelta ||
			sd.MaxAge < 0 || rd.MaxStale < 0 || rd.MinFresh < 0 {
			t.Fatalf("delta out of range: %+v %+v", rd, sd)
		}
		if sd.NoCache && sd.NoCacheFields != nil {
			// Both forms may appear across members; the unqualified one must dominate downstream.
			res := Resp{Status: 200, Header: h, RequestTime: t0, ResponseTime: t0, Lifetime: time.Hour}
			if d := (Policy{}).Evaluate(t0, res, sd, ReqDirectives{}); d.Action == Fresh {
				t.Fatalf("unqualified no-cache served fresh: %+v", d)
			}
		}
		ParseDate(cc, t0)
		res := Resp{Status: 200, Header: http.Header{"Cache-Control": {cc}, "Expires": {pragma}, "Date": {cc}, "Last-Modified": {pragma}, "Age": {cc}}, RequestTime: t0, ResponseTime: t0}
		res.Lifetime, res.Heuristic = (Policy{}).Lifetime(res, sd)
		(Policy{}).Storable(getReq(), rd, res, sd)
		(Policy{}).Evaluate(t0, res, sd, rd)
		NotModified(http.Header{"If-None-Match": {cc}, "If-Modified-Since": {pragma}}, http.Header{"ETag": {pragma}, "Last-Modified": {cc}})
	})
}

func BenchmarkParseResponse(b *testing.B) {
	h := hdr(`Cache-Control: public, max-age=3600, s-maxage=7200, stale-while-revalidate=60, no-cache="Set-Cookie"`)
	b.ReportAllocs()
	for b.Loop() {
		_ = ParseResponse(h)
	}
}

func BenchmarkParseRequest(b *testing.B) {
	h := hdr("Cache-Control: max-age=0, no-cache, max-stale=30")
	b.ReportAllocs()
	for b.Loop() {
		_ = ParseRequest(h)
	}
}

func BenchmarkParseDate(b *testing.B) {
	s := httpDate(t0)
	b.ReportAllocs()
	for b.Loop() {
		_, _ = ParseDate(s, t0)
	}
}

func BenchmarkAge(b *testing.B) {
	res := resp(200, time.Minute, "Age: 5")
	now := t0.Add(10 * time.Second)
	b.ReportAllocs()
	for b.Loop() {
		_ = Age(res, now)
	}
}

func BenchmarkEvaluateFresh(b *testing.B) {
	res := resp(200, time.Hour, "Cache-Control: max-age=3600", "ETag: \"v1\"")
	sd := ParseResponse(res.Header)
	now := t0.Add(10 * time.Second)
	var p Policy
	b.ReportAllocs()
	for b.Loop() {
		_ = p.Evaluate(now, res, sd, ReqDirectives{})
	}
}

func BenchmarkStorable(b *testing.B) {
	req := getReq("Accept: */*")
	res := resp(200, 0, "Cache-Control: max-age=3600", "ETag: \"v1\"", "Vary: Accept-Encoding")
	sd := ParseResponse(res.Header)
	var p Policy
	b.ReportAllocs()
	for b.Loop() {
		_, _ = p.Storable(req, ReqDirectives{}, res, sd)
	}
}
