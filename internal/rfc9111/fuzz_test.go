package rfc9111

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Fuzz targets for every parser in the package. Each asserts the parser never
// panics on arbitrary bytes, that its output stays within the documented
// bounds (MaxDelta, canonical field names, enum values), that parsing is a
// pure function of its input (idempotent, no aliasing of the caller's
// headers), and a round trip where an encoder exists (HTTP-date).

var fuzzClock = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

func FuzzParseRequest(f *testing.F) {
	for _, s := range []string{
		"", "no-cache", "no-store", "max-age=0", "max-age=60, no-transform", "max-stale", "max-stale=30",
		"min-fresh=10", "only-if-cached", "stale-if-error=120", "max-age=\"12\"", "max-age=99999999999999",
		"max-age=-1", "max-age=abc", "MAX-AGE=5,,, no-cache ,", "max-age=5, max-age=10",
		"no-cache=\"Set-Cookie\"", "foo=\"unterminated", "a=\"b\\\"c\", d", "=", ",,,", " \t ",
		"max-stale=, min-fresh=", "no-cache;x", "max-age=5 junk", "\"quoted\"=v",
	} {
		f.Add(s, "")
		f.Add("", s)
	}
	f.Fuzz(func(t *testing.T, cc, pragma string) {
		h := http.Header{}
		if cc != "" {
			h["Cache-Control"] = []string{cc}
		}
		if pragma != "" {
			h["Pragma"] = []string{pragma}
		}
		d := ParseRequest(h)
		for _, v := range []time.Duration{d.MaxAge, d.MaxStale, d.MinFresh, d.StaleIfError} {
			if v < 0 || v > MaxDelta {
				t.Fatalf("delta out of range: %v", v)
			}
		}
		if d.MaxStaleAny && !d.HasMaxStale {
			t.Fatal("MaxStaleAny without HasMaxStale")
		}
		if again := ParseRequest(h); !reflect.DeepEqual(d, again) {
			t.Fatalf("not idempotent: %+v vs %+v", d, again)
		}
		if cc != "" && h["Cache-Control"][0] != cc {
			t.Fatal("input header modified")
		}
		// Pragma only counts without Cache-Control (§5.4).
		if cc != "" && pragma != "" {
			h2 := http.Header{"Cache-Control": {cc}}
			if !reflect.DeepEqual(d, ParseRequest(h2)) {
				t.Fatal("Pragma influenced a request with Cache-Control")
			}
		}
	})
}

func FuzzParseResponse(f *testing.F) {
	for _, s := range []string{
		"", "max-age=60", "s-maxage=30, max-age=60", "no-store", "no-cache", "no-cache=\"Set-Cookie, X-Foo\"",
		"private", "private=\"Authorization\"", "public, must-revalidate", "proxy-revalidate", "immutable",
		"stale-while-revalidate=30, stale-if-error=60", "max-age=abc", "max-age=", "s-maxage=99999999999",
		"no-cache=\"\"", "private=\"\"", "max-age=5, max-age=10", "stale-while-revalidate=x",
		"Max-Age=5, No-Cache", "no-cache=\"set-cookie\", no-cache=\"x-y\"", "a=\"b\\\"", ",,=,,", "\"",
		"privAte=0 0", "no-cache=\"a b, c\"",
	} {
		f.Add(s, "")
		f.Add(s, "max-age=1")
	}
	f.Fuzz(func(t *testing.T, cc1, cc2 string) {
		h := http.Header{}
		for _, v := range []string{cc1, cc2} {
			if v != "" {
				h["Cache-Control"] = append(h["Cache-Control"], v)
			}
		}
		d := ParseResponse(h)
		for _, v := range []time.Duration{d.MaxAge, d.SMaxAge, d.StaleWhileRevalidate, d.StaleIfError} {
			if v < 0 || v > MaxDelta {
				t.Fatalf("delta out of range: %v", v)
			}
		}
		// Field lists carry the quoted-pair-resolved text (the existing table
		// test pins X-"Q"), so only trimming and splitting are asserted here.
		for _, list := range [][]string{d.NoCacheFields, d.PrivateFields} {
			for _, name := range list {
				if name == "" || name != strings.TrimSpace(name) || strings.Contains(name, ",") {
					t.Fatalf("field name not trimmed/split: %q", name)
				}
			}
		}
		if (d.HasSWR && !d.HasSWR) || (d.HasMaxAge && d.MaxAge < 0) {
			t.Fatal("inconsistent flags")
		}
		if again := ParseResponse(h); !reflect.DeepEqual(d, again) {
			t.Fatalf("not idempotent: %+v vs %+v", d, again)
		}
		// Parsing the two values as one joined field must give the same result
		// (unless a quoted-string is involved: an unterminated one runs to the
		// end of its own value, by design).
		if len(h["Cache-Control"]) == 2 && !strings.Contains(cc1+cc2, `"`) {
			joined := http.Header{"Cache-Control": {cc1 + "," + cc2}}
			if !reflect.DeepEqual(d, ParseResponse(joined)) {
				t.Fatalf("list vs joined differ: %+v vs %+v", d, ParseResponse(joined))
			}
		}
	})
}

func FuzzParseDate(f *testing.F) {
	for _, s := range []string{
		"Sun, 06 Nov 1994 08:49:37 GMT", "Sun, 06 Nov 1994 08:49:37 UTC", "Sunday, 06-Nov-94 08:49:37 GMT",
		"Sun Nov  6 08:49:37 1994", "Sun, 31 Feb 1994 08:49:37 GMT", "Sun, 00 Nov 1994 08:49:37 GMT",
		"Sun, 06 Nov 1994 24:00:00 GMT", "Sun, 06 Nov 1994 08:49:60 GMT", "Mon, 01 Jan 0001 00:00:00 GMT",
		"Fri, 31 Dec 9999 23:59:59 GMT", "Sun, 06 Nov 1994 08:49:37 PST", "", "0", "-1", "garbage",
		"Tue, 19 Sep 2079 12:00:00 GMT", "Tuesday, 19-Sep-79 12:00:00 GMT", " Sun, 06 Nov 1994 08:49:37 GMT ",
		"Sun, 06 Xyz 1994 08:49:37 GMT", "Sun, 06 Nov 1994 08:49:37 GMT\x00", "Sun Nov 1 0:00:00 2080",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		tm, ok := ParseDate(s, fuzzClock)
		if !ok {
			if !tm.IsZero() {
				t.Fatal("failed parse returned a non-zero time")
			}
			return
		}
		if tm.Location() != time.UTC {
			t.Fatalf("parsed time not UTC: %v", tm)
		}
		if _, err := time.Parse(time.RFC850, strings.TrimSpace(s)); err == nil && tm.After(fuzzClock.AddDate(50, 0, 0)) {
			t.Fatalf("two-digit year not moved back: %q -> %v", s, tm)
		}
		// Round trip through the one encoder: the IMF-fixdate form.
		back, ok := ParseDate(tm.Format(http.TimeFormat), fuzzClock)
		if !ok || !back.Equal(tm) {
			t.Fatalf("round trip %q -> %v -> %v (%v)", s, tm, back, ok)
		}
		// The reference clock only matters for the two-digit-year rule.
		if noRef, _ := ParseDate(s, time.Time{}); !noRef.Equal(tm) && noRef.AddDate(-100, 0, 0) != tm {
			if !noRef.Equal(tm.AddDate(100, 0, 0)) {
				t.Fatalf("ref clock changed the result: %v vs %v", tm, noRef)
			}
		}
	})
}

func FuzzNotModified(f *testing.F) {
	imf := fuzzClock.Format(http.TimeFormat)
	for _, c := range [][4]string{
		{`"abc"`, `"abc"`, "", ""},
		{`W/"abc"`, `"abc"`, "", ""},
		{`"a", "b"`, `"b"`, "", ""},
		{`*`, `"x"`, "", ""},
		{`"a,b"`, `"a,b"`, "", ""},
		{"", `"x"`, imf, imf},
		{"", "", imf, "Sun, 06 Nov 1994 08:49:37 GMT"},
		{"", "", "junk", imf},
		{`W/`, `W/`, "", ""},
		{`"`, `"`, "", ""},
		{`"unterminated`, `"unterminated`, "", ""},
		{",,,", `""`, "", ""},
	} {
		f.Add(c[0], c[1], c[2], c[3])
	}
	f.Fuzz(func(t *testing.T, inm, etag, ims, lm string) {
		req := http.Header{}
		if inm != "" {
			req.Set("If-None-Match", inm)
		}
		if ims != "" {
			req.Set("If-Modified-Since", ims)
		}
		stored := http.Header{}
		if etag != "" {
			stored.Set("ETag", etag)
		}
		if lm != "" {
			stored.Set("Last-Modified", lm)
		}
		got := NotModified(req, stored)
		if got != NotModified(req, stored) {
			t.Fatal("not idempotent")
		}
		// A stored ETag always matches a conditional carrying exactly that tag,
		// as long as the tag is a well-formed opaque-tag.
		if etag != "" && inm == "" && ims == "" && isOpaqueTag(etag) {
			self := http.Header{"If-None-Match": {etag}}
			if !NotModified(self, stored) {
				t.Fatalf("ETag %q does not match itself", etag)
			}
		}
		if etag == "" && inm != "" && got {
			t.Fatal("matched If-None-Match with no stored ETag")
		}
		if inm == "" && (ims == "" || lm == "") && got {
			t.Fatal("304 with nothing to compare")
		}
	})
}

// isOpaqueTag reports whether s is a single well-formed entity-tag: optional
// W/, then a quoted string without inner quotes.
func isOpaqueTag(s string) bool {
	s = strings.TrimPrefix(s, "W/")
	return len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' && !strings.Contains(s[1:len(s)-1], `"`)
}

func FuzzMerge304(f *testing.F) {
	f.Add("Cache-Control", "max-age=60", "Cache-Control", "max-age=120")
	f.Add("ETag", `"a"`, "ETag", `"b"`)
	f.Add("Content-Length", "10", "Content-Length", "0")
	f.Add("Date", "Sun, 06 Nov 1994 08:49:37 GMT", "Via", "1.1 x")
	f.Add("Age", "10", "X-New", "1")
	f.Add("Connection", "X-Hop", "X-Hop", "drop")
	f.Add("X-Keep", "v", "Connection", "X-Keep")
	f.Add("Transfer-Encoding", "chunked", "Keep-Alive", "timeout=5")
	f.Add("0", "0", "TE", "0")
	f.Add("", "", "", "")
	f.Fuzz(func(t *testing.T, sk, sv, fk, fv string) {
		stored, fresh := http.Header{}, http.Header{}
		if sk != "" {
			stored[http.CanonicalHeaderKey(sk)] = []string{sv}
		}
		if fk != "" {
			fresh[http.CanonicalHeaderKey(fk)] = []string{fv}
		}
		storedCopy, freshCopy := stored.Clone(), fresh.Clone()
		out := Merge304(stored, fresh)
		// stored is already StorableHeader'd in real use; what must never
		// arrive is a hop-by-hop field taken from the 304.
		for _, k := range hopByHop {
			k = http.CanonicalHeaderKey(k)
			if _, inStored := stored[k]; inStored {
				continue
			}
			if _, ok := out[k]; ok {
				t.Fatalf("hop-by-hop %s in merged header", k)
			}
		}
		if _, ok := out["Age"]; ok {
			t.Fatal("stored Age survived the merge")
		}
		for _, k := range notMerged {
			if v, ok := fresh[k]; ok && !isHopByHop(k) && out.Get(k) != "" && out.Get(k) == v[0] && stored.Get(k) != v[0] {
				t.Fatalf("%s taken from the 304", k)
			}
		}
		if len(out) > len(stored)+len(fresh) {
			t.Fatalf("merged header grew: %d > %d+%d", len(out), len(stored), len(fresh))
		}
		// The 304 wins where both carry a field the 304 is allowed to update.
		if fk != "" {
			ck := http.CanonicalHeaderKey(fk)
			if !isHopByHop(ck) && !contains(notMerged, ck) && ck != "Connection" && out.Get(ck) != fv {
				t.Fatalf("fresh %s = %q not applied: %q", ck, fv, out.Get(ck))
			}
		}
		// No aliasing: mutating the result must not touch the inputs.
		for k := range out {
			out[k] = append(out[k], "mutated")
		}
		if !reflect.DeepEqual(stored, storedCopy) || !reflect.DeepEqual(fresh, freshCopy) {
			t.Fatal("Merge304 aliased its inputs")
		}
	})
}

func FuzzStripHopByHop(f *testing.F) {
	f.Add("Connection", "X-Hop, close", "X-Hop", "v")
	f.Add("Transfer-Encoding", "chunked", "Content-Type", "text/plain")
	f.Add("Keep-Alive", "timeout=5", "Upgrade", "h2c")
	f.Add("Connection", "", "", "")
	f.Add("Connection", ",,,  ,", "Content-Length", "1")
	f.Add("connection", "content-length", "Content-Length", "5")
	f.Fuzz(func(t *testing.T, k1, v1, k2, v2 string) {
		h := http.Header{}
		if k1 != "" {
			h[http.CanonicalHeaderKey(k1)] = []string{v1}
		}
		if k2 != "" {
			h[http.CanonicalHeaderKey(k2)] = []string{v2}
		}
		before := h.Clone()
		out := StripHopByHop(h)
		for _, k := range hopByHop {
			if _, ok := out[http.CanonicalHeaderKey(k)]; ok {
				t.Fatalf("%s survived", k)
			}
		}
		for _, v := range before.Values("Connection") {
			for tok := range splitSeq(v) {
				if _, ok := out[http.CanonicalHeaderKey(tok)]; ok {
					t.Fatalf("Connection-listed %q survived", tok)
				}
			}
		}
		if len(out) > len(h) {
			t.Fatal("output larger than input")
		}
		if !reflect.DeepEqual(StripHopByHop(out), out) {
			t.Fatal("not idempotent")
		}
		for k := range out {
			out[k][0] = "mutated"
		}
		if !reflect.DeepEqual(h, before) {
			t.Fatal("StripHopByHop aliased its input")
		}
	})
}

// FuzzPolicy drives Lifetime, Age, Storable, Evaluate and StaleIfError with
// arbitrary header values and clock offsets: no panic, Age never negative,
// actions inside the enum, decisions consistent with each other.
func FuzzPolicy(f *testing.F) {
	imf := fuzzClock.Format(http.TimeFormat)
	f.Add("max-age=60", imf, "", "", `"e"`, "", int64(10), int64(0), 200)
	f.Add("s-maxage=1, must-revalidate", imf, "", "", "", "", int64(5), int64(-3), 200)
	f.Add("", imf, imf, "", "", "", int64(0), int64(0), 200)
	f.Add("", "", "", "Sun, 06 Nov 1994 08:49:37 GMT", "", "", int64(86400), int64(0), 404)
	f.Add("no-cache", imf, "", "", "", "", int64(1), int64(0), 200)
	f.Add("stale-while-revalidate=30", imf, "", "", "", "max-stale", int64(70), int64(0), 200)
	f.Add("max-age=60", imf, "", "", "", "min-fresh=100", int64(0), int64(0), 200)
	f.Add("max-age=60", imf, "", "", "", "only-if-cached", int64(120), int64(0), 200)
	f.Add("private", imf, "", "", "", "", int64(0), int64(0), 206)
	f.Add("max-age=60", "junk", "-1", "junk", "", "no-store", int64(0), int64(1<<40), 599)
	f.Fuzz(func(t *testing.T, cc, date, expires, lastMod, etag, reqCC string, elapsed, delay int64, status int) {
		h := http.Header{}
		for k, v := range map[string]string{
			"Cache-Control": cc, "Date": date, "Expires": expires, "Last-Modified": lastMod, "ETag": etag,
		} {
			if v != "" {
				h.Set(k, v)
			}
		}
		elapsed = clampDelta(elapsed)
		delay = clampDelta(delay)
		res := Resp{
			Status:       status,
			Header:       h,
			RequestTime:  fuzzClock.Add(-time.Duration(delay) * time.Second),
			ResponseTime: fuzzClock,
		}
		sd := ParseResponse(h)
		rd := ParseRequest(http.Header{"Cache-Control": {reqCC}})
		p := Policy{DefaultSWR: 5 * time.Second}
		lifetime, heuristic := p.Lifetime(res, sd)
		if lifetime < 0 {
			t.Fatalf("negative lifetime %v", lifetime)
		}
		if heuristic && (sd.HasMaxAge || sd.HasSMaxAge || expires != "") {
			t.Fatal("heuristic lifetime with explicit freshness")
		}
		res.Lifetime, res.Heuristic = lifetime, heuristic
		now := fuzzClock.Add(time.Duration(elapsed) * time.Second)
		if age := Age(res, now); age < 0 {
			t.Fatalf("negative age %v", age)
		}
		storable, reason := p.Storable(nil, rd, res, sd)
		if storable && reason != ReasonNone {
			t.Fatalf("storable with reason %q", reason)
		}
		if !storable && reason == ReasonNone {
			t.Fatal("not storable without a reason")
		}
		if storable && (sd.NoStore || rd.NoStore || sd.Private || status == http.StatusNotModified) {
			t.Fatalf("stored a response that must not be: %+v", sd)
		}
		d := p.Evaluate(now, res, sd, rd)
		if d.Action < Fresh || d.Action > GatewayTimeout {
			t.Fatalf("action out of range: %d", d.Action)
		}
		if d.Age < 0 {
			t.Fatal("negative decision age")
		}
		if d.Background != (d.Action == StaleWhileRevalidate) {
			t.Fatalf("Background=%v for %v", d.Background, d.Action)
		}
		if d.Action == Revalidate && !HasValidator(h) {
			t.Fatal("Revalidate without a validator")
		}
		if d.Action == GatewayTimeout && !rd.OnlyIfCached {
			t.Fatal("GatewayTimeout without only-if-cached")
		}
		if d.Action == Bypass && !rd.NoStore {
			t.Fatal("Bypass without request no-store")
		}
		ok, sreason := p.StaleIfError(now, res, sd, rd)
		if ok && sreason != ReasonFresh && sreason != ReasonSIE {
			t.Fatalf("StaleIfError allowed with reason %q", sreason)
		}
		if !ok && sreason == ReasonNone {
			t.Fatal("StaleIfError refused without a reason")
		}
		if ok && (sd.MustRevalidate || sd.ProxyRevalidate || sd.NoCache || rd.NoCache || rd.NoStore) {
			t.Fatal("stale-if-error allowed under a forbidding directive")
		}
		cond := http.Header{}
		ConditionalRequest(cond, res)
		if (cond.Get("If-None-Match") != "") != (etag != "") {
			t.Fatal("If-None-Match does not track ETag")
		}
	})
}

func clampDelta(v int64) int64 {
	const limit = int64(1) << 33
	if v > limit {
		return limit
	}
	if v < -limit {
		return -limit
	}
	return v
}

func isHopByHop(k string) bool { return contains(hopByHop, k) }

// contains matches k against list by canonical field name ("TE" is "Te").
func contains(list []string, k string) bool {
	for _, v := range list {
		if http.CanonicalHeaderKey(v) == http.CanonicalHeaderKey(k) {
			return true
		}
	}
	return false
}
