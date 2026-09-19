// Package rfc9111 is pure HTTP caching policy (RFC 9111, RFC 9110 §13, RFC
// 5861): header bytes in, decisions out. It performs no I/O and owns no clock;
// `now time.Time` is always a parameter so every rule is a table test.
//
// The engine's call order is: ParseRequest on the way in; on a miss
// ParseResponse -> Storable -> Lifetime -> StorableHeader before writing; on a
// hit Evaluate (which computes Age), then NotModified if the client sent its
// own conditional; on Revalidate ConditionalRequest, then Merge304 +
// StorableHeader + Lifetime for a 304; on a failed revalidation StaleIfError
// decides whether the stale entry may still be served.
package rfc9111

import (
	"net/http"
	"strings"
	"time"
)

// ReqDirectives is the parsed request Cache-Control (§5.2.1). Has* flags tell an
// absent directive from a zero value; MaxStaleAny is a bare "max-stale".
// StaleIfError is RFC 5861 §4's request form.
type ReqDirectives struct {
	NoStore, NoCache, OnlyIfCached, NoTransform bool
	MaxAge, MaxStale, MinFresh                  time.Duration
	HasMaxAge, HasMaxStale, MaxStaleAny         bool
	HasMinFresh                                 bool
	StaleIfError                                time.Duration
	HasStaleIfError                             bool
}

// RespDirectives is the parsed response Cache-Control (§5.2.2 + RFC 5861).
// NoCacheFields / PrivateFields carry the qualified forms (no-cache="Set-Cookie")
// as canonical field names; a qualified form alone does not set NoCache /
// Private, because §5.2.2.4 and §5.2.2.7 let a shared cache store the rest of
// the response once those fields are stripped (see StorableHeader).
type RespDirectives struct {
	NoStore, NoCache, Private, Public, MustRevalidate, ProxyRevalidate, Immutable bool
	NoCacheFields, PrivateFields                                                  []string
	MaxAge, SMaxAge, StaleWhileRevalidate, StaleIfError                           time.Duration
	HasMaxAge, HasSMaxAge, HasSWR, HasSIE                                         bool
}

// Resp is what the store persists about one origin exchange. Header has
// hop-by-hop fields stripped; RequestTime/ResponseTime are §4.2.3's
// request_time / response_time; Lifetime is the computed freshness lifetime and
// Heuristic says it came from §4.2.2 rather than an explicit directive.
type Resp struct {
	Status                    int
	Header                    http.Header
	RequestTime, ResponseTime time.Time
	Lifetime                  time.Duration
	Heuristic                 bool
}

// Policy is the injected (never global) tunable part of the spec: heuristic
// freshness is HeuristicFraction of (Date - Last-Modified) capped at
// HeuristicMax; CacheAuthorization relaxes §3.5 for an origin you own.
// DefaultSWR / DefaultSIE are the RFC 5861 windows applied only when the
// response carries neither directive (config DEFAULT_SWR / DEFAULT_SIE). A zero
// HeuristicFraction or HeuristicMax means the RFC defaults (0.1, 24h).
type Policy struct {
	HeuristicFraction  float64
	HeuristicMax       time.Duration
	CacheAuthorization bool
	DefaultSWR         time.Duration
	DefaultSIE         time.Duration
}

// Defaults for a zero Policy: §4.2.2's suggested 10 % and the customary 24h cap.
const (
	DefaultHeuristicFraction = 0.1
	DefaultHeuristicMax      = 24 * time.Hour
)

// MaxDelta is the §1.2.2 ceiling: a delta-seconds value that overflows
// is treated as 2^31 seconds.
const MaxDelta = time.Duration(1<<31) * time.Second

// Action is what the engine must do with a stored response.
type Action int

// Actions returned by Policy.Evaluate.
const (
	// Fresh serves the stored response as is.
	Fresh Action = iota
	// Stale means the entry is past its lifetime and nothing allows serving it; revalidate or fetch.
	Stale
	// Revalidate requires a conditional request before the entry can be used.
	Revalidate
	// StaleWhileRevalidate serves the stale entry now and refreshes in the background (RFC 5861).
	StaleWhileRevalidate
	// Bypass means the request must go to the origin uncached.
	Bypass
	// GatewayTimeout answers 504: only-if-cached with no usable entry.
	GatewayTimeout
)

// String returns the lowercase action name used in logs and X-Cache-Reason.
func (a Action) String() string {
	switch a {
	case Fresh:
		return "fresh"
	case Stale:
		return "stale"
	case Revalidate:
		return "revalidate"
	case StaleWhileRevalidate:
		return "stale-while-revalidate"
	case Bypass:
		return "bypass"
	case GatewayTimeout:
		return "gateway-timeout"
	}
	return "unknown"
}

// Reason names why a decision was taken; it is emitted as X-Cache-Reason on
// bypass and in the request log. Values are stable strings, never free text.
type Reason string

// Reasons produced by Storable and Evaluate.
const (
	ReasonNone             Reason = ""
	ReasonFresh            Reason = "fresh"
	ReasonExpired          Reason = "expired"
	ReasonHeuristic        Reason = "heuristic"
	ReasonMethod           Reason = "method"
	ReasonUnsafeMethod     Reason = "unsafe-method"
	ReasonStatus           Reason = "status"
	ReasonReqNoStore       Reason = "request-no-store"
	ReasonRespNoStore      Reason = "response-no-store"
	ReasonReqNoCache       Reason = "request-no-cache"
	ReasonRespNoCache      Reason = "response-no-cache"
	ReasonPrivate          Reason = "private"
	ReasonVaryStar         Reason = "vary-star"
	ReasonNoFreshness      Reason = "no-freshness"
	ReasonAuthorization    Reason = "authorization"
	ReasonRange            Reason = "range"
	ReasonMaxAge           Reason = "max-age"
	ReasonMaxStale         Reason = "max-stale"
	ReasonMinFresh         Reason = "min-fresh"
	ReasonOnlyIfCached     Reason = "only-if-cached"
	ReasonMustRevalidate   Reason = "must-revalidate"
	ReasonSWR              Reason = "stale-while-revalidate"
	ReasonSIE              Reason = "stale-if-error"
	ReasonTooLarge         Reason = "too-large"
	ReasonConditional      Reason = "conditional"
	ReasonNotImplemented   Reason = "stub"
	ReasonImmutableRefresh Reason = "immutable"
)

// Decision is the outcome of Policy.Evaluate: the action, the current Age (§4.2.3)
// to send, whether a background revalidation should be started, and why.
type Decision struct {
	Action     Action
	Age        time.Duration
	Background bool
	Reason     Reason
}

// ---------------------------------------------------------------------------
// Cache-Control tokenizer (§5.2): #cache-directive, directive = token [ "="
// ( token / quoted-string ) ], names compared case-insensitively.

// eachDirective calls fn for every directive in the given field values, in
// order. arg is the unquoted argument (quoted-pairs resolved) and hasArg tells
// "max-stale" from "max-stale=". Malformed input never panics: an unterminated
// quoted-string runs to the end of the value.
func eachDirective(values []string, fn func(name, arg string, hasArg bool)) {
	for _, s := range values {
		i := 0
		for i < len(s) {
			for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == ',') {
				i++
			}
			if i >= len(s) {
				break
			}
			start := i
			for i < len(s) && s[i] != '=' && s[i] != ',' {
				i++
			}
			name := strings.TrimRight(s[start:i], " \t")
			if i >= len(s) || s[i] == ',' {
				fn(name, "", false)
				continue
			}
			i++ // '='
			for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
				i++
			}
			if i < len(s) && s[i] == '"' {
				i++
				argStart, escaped := i, false
				for i < len(s) && s[i] != '"' {
					if s[i] == '\\' && i+1 < len(s) {
						escaped = true
						i++
					}
					i++
				}
				arg := s[argStart:i]
				if escaped {
					arg = unescapeQuotedPairs(arg)
				}
				if i < len(s) {
					i++ // closing quote
				}
				for i < len(s) && s[i] != ',' {
					i++ // junk after the quoted-string
				}
				fn(name, arg, true)
				continue
			}
			argStart := i
			for i < len(s) && s[i] != ',' {
				i++
			}
			fn(name, strings.TrimRight(s[argStart:i], " \t"), true)
		}
	}
}

// unescapeQuotedPairs resolves "\x" -> "x" inside a quoted-string's content.
func unescapeQuotedPairs(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseDelta parses §1.2.2 delta-seconds: one or more DIGITs, capped at 2^31.
func parseDelta(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	var n int64
	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		if n < 1<<31 {
			n = n*10 + int64(c-'0')
		}
	}
	if n > 1<<31 {
		n = 1 << 31
	}
	return time.Duration(n) * time.Second, true
}

// parseFieldList splits a qualified directive's argument ("Set-Cookie, X-Foo")
// into canonical field names; nil for an empty list.
func parseFieldList(arg string) []string {
	var out []string
	for part := range strings.SplitSeq(arg, ",") {
		if name := strings.TrimSpace(part); name != "" {
			out = append(out, http.CanonicalHeaderKey(name))
		}
	}
	return out
}

// ParseRequest parses the request's Cache-Control (quoted-string aware) and,
// when no Cache-Control is present, Pragma: no-cache (§5.4). Duplicated valued
// directives keep the first occurrence; an invalid max-age counts as max-age=0
// (forces revalidation) while an invalid max-stale / min-fresh / stale-if-error
// is ignored, i.e. the conservative reading each time.
func ParseRequest(h http.Header) ReqDirectives {
	var d ReqDirectives
	cc := h.Values("Cache-Control")
	if len(cc) == 0 {
		eachDirective(h.Values("Pragma"), func(name, _ string, _ bool) {
			if strings.EqualFold(name, "no-cache") {
				d.NoCache = true
			}
		})
		return d
	}
	eachDirective(cc, func(name, arg string, hasArg bool) {
		switch {
		case strings.EqualFold(name, "no-store"):
			d.NoStore = true
		case strings.EqualFold(name, "no-cache"):
			d.NoCache = true
		case strings.EqualFold(name, "only-if-cached"):
			d.OnlyIfCached = true
		case strings.EqualFold(name, "no-transform"):
			d.NoTransform = true
		case strings.EqualFold(name, "max-age"):
			if d.HasMaxAge {
				return
			}
			d.HasMaxAge = true
			d.MaxAge, _ = parseDelta(arg)
		case strings.EqualFold(name, "max-stale"):
			if d.HasMaxStale {
				return
			}
			if !hasArg {
				d.HasMaxStale, d.MaxStaleAny = true, true
				return
			}
			if v, ok := parseDelta(arg); ok {
				d.HasMaxStale, d.MaxStale = true, v
			}
		case strings.EqualFold(name, "min-fresh"):
			if d.HasMinFresh {
				return
			}
			if v, ok := parseDelta(arg); ok {
				d.HasMinFresh, d.MinFresh = true, v
			}
		case strings.EqualFold(name, "stale-if-error"):
			if d.HasStaleIfError {
				return
			}
			if v, ok := parseDelta(arg); ok {
				d.HasStaleIfError, d.StaleIfError = true, v
			}
		}
	})
	return d
}

// ParseResponse parses the response's Cache-Control, including qualified
// no-cache="..." / private="..." forms and RFC 5861 extensions. Duplicated
// valued directives keep the first occurrence (§4.2.1); an invalid max-age /
// s-maxage counts as 0 (the response is stale, per §4.2.1's advice), an invalid
// stale-while-revalidate / stale-if-error is ignored.
func ParseResponse(h http.Header) RespDirectives {
	var d RespDirectives
	eachDirective(h.Values("Cache-Control"), func(name, arg string, hasArg bool) {
		switch {
		case strings.EqualFold(name, "no-store"):
			d.NoStore = true
		case strings.EqualFold(name, "no-cache"):
			if fields := qualifiedFields(arg, hasArg); fields != nil {
				d.NoCacheFields = append(d.NoCacheFields, fields...)
			} else {
				d.NoCache = true
			}
		case strings.EqualFold(name, "private"):
			if fields := qualifiedFields(arg, hasArg); fields != nil {
				d.PrivateFields = append(d.PrivateFields, fields...)
			} else {
				d.Private = true
			}
		case strings.EqualFold(name, "public"):
			d.Public = true
		case strings.EqualFold(name, "must-revalidate"):
			d.MustRevalidate = true
		case strings.EqualFold(name, "proxy-revalidate"):
			d.ProxyRevalidate = true
		case strings.EqualFold(name, "immutable"):
			d.Immutable = true
		case strings.EqualFold(name, "max-age"):
			if !d.HasMaxAge {
				d.HasMaxAge = true
				d.MaxAge, _ = parseDelta(arg)
			}
		case strings.EqualFold(name, "s-maxage"):
			if !d.HasSMaxAge {
				d.HasSMaxAge = true
				d.SMaxAge, _ = parseDelta(arg)
			}
		case strings.EqualFold(name, "stale-while-revalidate"):
			if v, ok := parseDelta(arg); ok && !d.HasSWR {
				d.HasSWR, d.StaleWhileRevalidate = true, v
			}
		case strings.EqualFold(name, "stale-if-error"):
			if v, ok := parseDelta(arg); ok && !d.HasSIE {
				d.HasSIE, d.StaleIfError = true, v
			}
		}
	})
	return d
}

// qualifiedFields returns the field list of a qualified no-cache / private, or
// nil when the directive is unqualified (or its list is empty, which is read as
// the stricter unqualified form).
func qualifiedFields(arg string, hasArg bool) []string {
	if !hasArg {
		return nil
	}
	return parseFieldList(arg)
}

// ---------------------------------------------------------------------------
// HTTP-date (RFC 9110 §5.6.7): IMF-fixdate, obsolete RFC 850 and asctime.

var monthIndex = map[string]time.Month{
	"Jan": time.January, "Feb": time.February, "Mar": time.March, "Apr": time.April,
	"May": time.May, "Jun": time.June, "Jul": time.July, "Aug": time.August,
	"Sep": time.September, "Oct": time.October, "Nov": time.November, "Dec": time.December,
}

// ParseDate parses an HTTP-date in any of the three RFC 9110 §5.6.7 formats.
// ref is the receiver's notion of the current time, used only for the RFC 850
// two-digit-year rule (a date more than 50 years in the future is moved back a
// century); a zero ref skips that rule. The fast path for IMF-fixdate does not
// allocate. Invalid input returns ok == false.
func ParseDate(s string, ref time.Time) (t time.Time, ok bool) {
	s = strings.TrimSpace(s)
	if t, ok = parseIMF(s); ok {
		return t, true
	}
	if t, err := time.Parse(time.RFC850, s); err == nil {
		if !ref.IsZero() && t.After(ref.AddDate(50, 0, 0)) {
			t = t.AddDate(-100, 0, 0)
		}
		return t.UTC(), true
	}
	if t, err := time.Parse(time.ANSIC, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// parseIMF is the alloc-free fast path for "Sun, 06 Nov 1994 08:49:37 GMT".
// "UTC" is tolerated in place of "GMT".
func parseIMF(s string) (time.Time, bool) {
	if len(s) != 29 || s[3] != ',' || s[4] != ' ' || s[7] != ' ' || s[11] != ' ' ||
		s[16] != ' ' || s[19] != ':' || s[22] != ':' || s[25] != ' ' ||
		(s[26:] != "GMT" && s[26:] != "UTC") {
		return time.Time{}, false
	}
	day, ok1 := atoi2(s[5:7])
	year, ok2 := atoi4(s[12:16])
	hour, ok3 := atoi2(s[17:19])
	minute, ok4 := atoi2(s[20:22])
	sec, ok5 := atoi2(s[23:25])
	month, ok6 := monthIndex[s[8:11]]
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || day == 0 || hour > 23 || minute > 59 || sec > 60 {
		return time.Time{}, false
	}
	t := time.Date(year, month, day, hour, minute, sec, 0, time.UTC)
	if t.Day() != day { // e.g. 31 Feb
		return time.Time{}, false
	}
	return t, true
}

func atoi2(s string) (int, bool) {
	if len(s) != 2 || s[0] < '0' || s[0] > '9' || s[1] < '0' || s[1] > '9' {
		return 0, false
	}
	return int(s[0]-'0')*10 + int(s[1]-'0'), true
}

func atoi4(s string) (int, bool) {
	if len(s) != 4 {
		return 0, false
	}
	n := 0
	for i := range 4 {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

// dateValue is §4.2.3's date_value: the Date header, or response_time when the
// field is absent or invalid (RFC 9110 §6.6.1 has the cache record its receipt
// time in that case; StorableHeader materialises it).
func dateValue(res Resp) time.Time {
	if v := res.Header.Get("Date"); v != "" {
		if t, ok := ParseDate(v, res.ResponseTime); ok {
			return t
		}
	}
	return res.ResponseTime
}

// ageValue is §4.2.3's age_value: the first Age field member, 0 when absent or
// invalid (§5.1), 2^31 s on overflow.
func ageValue(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Age"))
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	d, _ := parseDelta(v)
	return d
}

// HeuristicallyCacheable reports whether status is cacheable by default (RFC
// 9110 §15.1 plus RFC 7725 §3 for 451) and so may carry a §4.2.2 heuristic
// lifetime. 206 is excluded: partial content is never stored in v1.
func HeuristicallyCacheable(status int) bool {
	switch status {
	case http.StatusOK, http.StatusNonAuthoritativeInfo, http.StatusNoContent,
		http.StatusMultipleChoices, http.StatusMovedPermanently, http.StatusPermanentRedirect,
		http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusGone,
		http.StatusRequestURITooLong, http.StatusUnavailableForLegalReasons, http.StatusNotImplemented:
		return true
	}
	return false
}

// HasValidator reports whether h carries an ETag or Last-Modified, i.e. whether
// a stale entry can be refreshed with a conditional request (§4.3.1).
func HasValidator(h http.Header) bool {
	return h.Get("ETag") != "" || h.Get("Last-Modified") != ""
}

// varyStar reports whether any Vary member is "*" (§4.1).
func varyStar(h http.Header) bool {
	for _, v := range h.Values("Vary") {
		for part := range strings.SplitSeq(v, ",") {
			if strings.TrimSpace(part) == "*" {
				return true
			}
		}
	}
	return false
}

// hasExplicitFreshness is §3's "explicit freshness information": a max-age or
// s-maxage directive or an Expires field.
func hasExplicitFreshness(h http.Header, sd RespDirectives) bool {
	return sd.HasMaxAge || sd.HasSMaxAge || h.Get("Expires") != ""
}

// Storable applies §3: cacheable method and status, no no-store on either side,
// no Vary: *, not private, explicit or heuristic freshness, and the §3.5
// Authorization rule unless p.CacheAuthorization. The Reason explains a false.
//
// Beyond the letter of §3 it refuses entries that could never be served without
// a full fetch: an unqualified no-cache without a validator (ReasonRespNoCache)
// and a zero lifetime with no validator and no stale window (ReasonNoFreshness).
// A nil r skips the request-side checks (method, Authorization, Range).
func (p Policy) Storable(r *http.Request, rd ReqDirectives, res Resp, sd RespDirectives) (bool, Reason) {
	if r != nil {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			return false, ReasonUnsafeMethod
		default:
			return false, ReasonMethod
		}
	}
	switch {
	case rd.NoStore:
		return false, ReasonReqNoStore
	case sd.NoStore:
		return false, ReasonRespNoStore
	case sd.Private:
		return false, ReasonPrivate
	case res.Status < http.StatusOK || res.Status == http.StatusNotModified:
		return false, ReasonStatus
	case res.Status == http.StatusPartialContent || res.Header.Get("Content-Range") != "" ||
		(r != nil && r.Header.Get("Range") != ""):
		return false, ReasonRange
	case varyStar(res.Header):
		return false, ReasonVaryStar
	}
	if r != nil && r.Header.Get("Authorization") != "" && !p.CacheAuthorization &&
		!sd.Public && !sd.HasSMaxAge && !sd.MustRevalidate {
		return false, ReasonAuthorization
	}
	validator := HasValidator(res.Header)
	if sd.NoCache {
		if validator {
			return true, ReasonNone
		}
		return false, ReasonRespNoCache
	}
	if !hasExplicitFreshness(res.Header, sd) && !sd.Public && !HeuristicallyCacheable(res.Status) {
		return false, ReasonStatus
	}
	if lifetime, _ := p.Lifetime(res, sd); lifetime <= 0 && !validator &&
		!sd.HasSWR && !sd.HasSIE && p.DefaultSWR <= 0 && p.DefaultSIE <= 0 {
		return false, ReasonNoFreshness
	}
	return true, ReasonNone
}

// Lifetime computes the freshness lifetime (§4.2.1): s-maxage > max-age >
// Expires - Date > heuristic (§4.2.2). The bool reports a heuristic lifetime.
// An invalid Expires ("0", "-1", garbage) is "already expired" (§5.3) and
// yields 0. The heuristic needs a heuristically cacheable status and a
// Last-Modified before Date; it is HeuristicFraction of that span, capped at
// HeuristicMax.
func (p Policy) Lifetime(res Resp, sd RespDirectives) (time.Duration, bool) {
	switch {
	case sd.HasSMaxAge:
		return sd.SMaxAge, false
	case sd.HasMaxAge:
		return sd.MaxAge, false
	}
	if exp := res.Header.Values("Expires"); len(exp) > 0 {
		date := dateValue(res)
		t, ok := ParseDate(exp[0], date)
		if !ok {
			return 0, false
		}
		return max(t.Sub(date), 0), false
	}
	if !HeuristicallyCacheable(res.Status) {
		return 0, false
	}
	lmv := res.Header.Get("Last-Modified")
	if lmv == "" {
		return 0, false
	}
	date := dateValue(res)
	lm, ok := ParseDate(lmv, date)
	if !ok {
		return 0, false
	}
	span := date.Sub(lm)
	if span <= 0 {
		return 0, false
	}
	fraction, limit := p.HeuristicFraction, p.HeuristicMax
	if fraction <= 0 {
		fraction = DefaultHeuristicFraction
	}
	if limit <= 0 {
		limit = DefaultHeuristicMax
	}
	return min(time.Duration(float64(span)*fraction), limit), true
}

// Age computes the current age (§4.2.3) of res at now, including response_delay,
// clamped at 0:
//
//	apparent_age          = max(0, response_time - date_value)
//	corrected_age_value   = age_value + (response_time - request_time)
//	corrected_initial_age = max(apparent_age, corrected_age_value)
//	current_age           = corrected_initial_age + (now - response_time)
//
// A zero RequestTime or ResponseTime contributes 0 to its term.
func Age(res Resp, now time.Time) time.Duration {
	var apparent, delay, resident time.Duration
	if !res.ResponseTime.IsZero() {
		apparent = max(res.ResponseTime.Sub(dateValue(res)), 0)
		if !res.RequestTime.IsZero() {
			delay = max(res.ResponseTime.Sub(res.RequestTime), 0)
		}
		resident = now.Sub(res.ResponseTime)
	}
	initial := max(apparent, ageValue(res.Header)+delay)
	return max(initial+resident, 0)
}

// Evaluate decides how a stored response may be used at now: Fresh, Stale,
// Revalidate, StaleWhileRevalidate, Bypass or GatewayTimeout, honouring the
// request's max-age / max-stale / min-fresh / no-cache / only-if-cached and the
// response's must-revalidate / proxy-revalidate / swr / sie.
//
// Reading the result: Fresh with ReasonFresh is a plain HIT; Fresh with
// ReasonMaxStale is a stale entry the client agreed to (label it STALE).
// Revalidate means the entry has a validator, so send ConditionalRequest and
// expect a 304; Stale means it has none, so fetch unconditionally and replace
// it. StaleWhileRevalidate sets Background. Bypass only comes from a request
// no-store. GatewayTimeout is only-if-cached with nothing servable. Decision.Age
// is always filled, whatever the action.
func (p Policy) Evaluate(now time.Time, res Resp, sd RespDirectives, rd ReqDirectives) Decision {
	age := Age(res, now)
	d := Decision{Age: age}
	refresh := func(reason Reason) Decision {
		switch {
		case rd.OnlyIfCached:
			d.Action, d.Reason = GatewayTimeout, ReasonOnlyIfCached
		case HasValidator(res.Header):
			d.Action, d.Reason = Revalidate, reason
		default:
			d.Action, d.Reason = Stale, reason
		}
		return d
	}
	switch {
	case rd.NoStore:
		d.Action, d.Reason = Bypass, ReasonReqNoStore
		return d
	case sd.NoCache:
		return refresh(ReasonRespNoCache)
	case rd.NoCache:
		return refresh(ReasonReqNoCache)
	case rd.HasMaxAge && (rd.MaxAge == 0 || age > rd.MaxAge):
		return refresh(ReasonMaxAge)
	case rd.HasMinFresh && res.Lifetime < age+rd.MinFresh:
		return refresh(ReasonMinFresh)
	case age < res.Lifetime:
		d.Action, d.Reason = Fresh, ReasonFresh
		return d
	}
	staleness := age - res.Lifetime
	if sd.MustRevalidate || sd.ProxyRevalidate {
		return refresh(ReasonMustRevalidate)
	}
	if rd.HasMaxStale && (rd.MaxStaleAny || staleness <= rd.MaxStale) {
		d.Action, d.Reason = Fresh, ReasonMaxStale
		return d
	}
	swr := p.DefaultSWR
	if sd.HasSWR {
		swr = sd.StaleWhileRevalidate
	}
	if swr > 0 && staleness <= swr {
		d.Action, d.Reason, d.Background = StaleWhileRevalidate, ReasonSWR, true
		return d
	}
	if res.Heuristic {
		return refresh(ReasonHeuristic)
	}
	return refresh(ReasonExpired)
}

// StaleIfError applies RFC 5861 §4 after a revalidation or fetch failed (5xx
// or transport error): the stored entry may be served when its staleness is
// within the stale-if-error window of the response, the request, or
// p.DefaultSIE (the larger of those present), unless must-revalidate /
// proxy-revalidate, an unqualified no-cache on either side, or a request
// no-store forbids it. A fresh entry is always allowed.
func (p Policy) StaleIfError(now time.Time, res Resp, sd RespDirectives, rd ReqDirectives) (bool, Reason) {
	switch {
	case rd.NoStore:
		return false, ReasonReqNoStore
	case sd.NoCache:
		return false, ReasonRespNoCache
	case rd.NoCache:
		return false, ReasonReqNoCache
	case sd.MustRevalidate || sd.ProxyRevalidate:
		return false, ReasonMustRevalidate
	}
	staleness := Age(res, now) - res.Lifetime
	if staleness < 0 {
		return true, ReasonFresh
	}
	window := p.DefaultSIE
	if sd.HasSIE {
		window = sd.StaleIfError
	}
	if rd.HasStaleIfError {
		window = max(window, rd.StaleIfError)
	}
	if window > 0 && staleness <= window {
		return true, ReasonSIE
	}
	return false, ReasonExpired
}

// ConditionalRequest adds If-None-Match (stored ETag) and/or If-Modified-Since
// (stored Last-Modified) to h so the origin can answer 304. Existing values of
// those two fields are replaced; a nil h is a no-op.
func ConditionalRequest(h http.Header, res Resp) {
	if h == nil {
		return
	}
	if etags := res.Header.Values("ETag"); len(etags) > 0 {
		h.Set("If-None-Match", strings.Join(etags, ", "))
	}
	if lm := res.Header.Get("Last-Modified"); lm != "" {
		h.Set("If-Modified-Since", lm)
	}
}

// NotModified evaluates a client's own conditional request against a stored
// response (RFC 9110 §13.1): true when the engine may answer 304 from the
// entry instead of sending its body. If-None-Match uses weak comparison
// (§8.8.3.2) and "*" matches any stored ETag; If-Modified-Since is consulted
// only when If-None-Match is absent (§13.1.3) and only for GET/HEAD, which the
// caller has already ensured. Unparseable dates are ignored, i.e. modified.
func NotModified(req, stored http.Header) bool {
	if inm := req.Values("If-None-Match"); len(inm) > 0 {
		etag := stored.Get("ETag")
		if etag == "" {
			return false
		}
		for _, v := range inm {
			if strings.TrimSpace(v) == "*" {
				return true
			}
			for tag := range etagSeq(v) {
				if weakMatch(tag, etag) {
					return true
				}
			}
		}
		return false
	}
	ims := req.Get("If-Modified-Since")
	lmv := stored.Get("Last-Modified")
	if ims == "" || lmv == "" {
		return false
	}
	since, ok1 := ParseDate(ims, time.Time{})
	lm, ok2 := ParseDate(lmv, time.Time{})
	return ok1 && ok2 && !lm.After(since)
}

// weakMatch is §8.8.3.2 weak comparison: both tags equal octet-by-octet once
// any W/ prefix is removed.
func weakMatch(a, b string) bool {
	return strings.TrimPrefix(a, "W/") == strings.TrimPrefix(b, "W/")
}

// etagSeq yields the entity-tags of an If-None-Match list. Tags are scanned as
// quoted opaque-tags with an optional W/ prefix, so a comma inside a tag is
// not a separator; an unquoted token runs to the next comma.
func etagSeq(v string) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		i := 0
		for i < len(v) {
			for i < len(v) && (v[i] == ' ' || v[i] == '\t' || v[i] == ',') {
				i++
			}
			if i >= len(v) {
				return
			}
			start := i
			if strings.HasPrefix(v[i:], "W/") {
				i += 2
			}
			if i < len(v) && v[i] == '"' {
				i++
				for i < len(v) && v[i] != '"' {
					i++
				}
				if i < len(v) {
					i++
				}
			} else {
				for i < len(v) && v[i] != ',' {
					i++
				}
			}
			if !yield(strings.TrimRight(v[start:i], " \t")) {
				return
			}
		}
	}
}

// notMerged lists the fields a 304 never updates: Content-Length (§3.2) and the
// other body-describing fields, which belong to the stored body, not the 304.
var notMerged = []string{"Content-Length", "Content-Encoding", "Content-Range"}

// Merge304 applies §4.3.4 / §3.2: every field in fresh replaces the stored one;
// stored fields absent from fresh are kept. Hop-by-hop fields, fields named in
// fresh's Connection, Content-Length, Content-Encoding and Content-Range are
// never taken from the 304. Two time-relative fields are the exception to
// "never drop": a stored Age is discarded and a stored Date is dropped when the
// 304 carries none, so the refreshed RequestTime/ResponseTime become the
// entry's date_value. The result is a new Header; run it through
// StorableHeader before storing.
func Merge304(stored, fresh http.Header) http.Header {
	out := make(http.Header, len(stored)+len(fresh))
	for k, v := range stored {
		out[k] = append([]string(nil), v...)
	}
	out.Del("Age")
	if fresh.Get("Date") == "" {
		out.Del("Date")
	}
	upd := StripHopByHop(fresh)
	for _, k := range notMerged {
		upd.Del(k)
	}
	for k, v := range upd {
		out[k] = v
	}
	return out
}

// StorableHeader is what the engine stores: h without hop-by-hop fields, without
// the fields named by a qualified no-cache="..." / private="..." (§5.2.2.4,
// §5.2.2.7), without Warning (obsolete, never emitted) and with a Date set to
// received (RFC 9110 §6.6.1) when the origin sent none. It is idempotent, so
// call it after Merge304 too. h itself is not modified.
func StorableHeader(h http.Header, sd RespDirectives, received time.Time) http.Header {
	out := StripHopByHop(h)
	for _, f := range sd.NoCacheFields {
		out.Del(f)
	}
	for _, f := range sd.PrivateFields {
		out.Del(f)
	}
	out.Del("Warning")
	if out.Get("Date") == "" && !received.IsZero() {
		out.Set("Date", received.UTC().Format(http.TimeFormat))
	}
	return out
}

// hopByHop lists the fields RFC 9110 §7.6.1 forbids forwarding, plus the
// connection-specific ones a cache must never store.
var hopByHop = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// StripHopByHop returns a copy of h without the hop-by-hop fields and without
// every field named in a Connection header. h itself is not modified; a nil h
// yields an empty Header.
func StripHopByHop(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		out[k] = append([]string(nil), v...)
	}
	for _, v := range h.Values("Connection") {
		for tok := range splitSeq(v) {
			out.Del(tok)
		}
	}
	for _, k := range hopByHop {
		out.Del(k)
	}
	return out
}

// splitSeq yields the OWS-trimmed, non-empty comma-separated tokens of v.
func splitSeq(v string) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for part := range strings.SplitSeq(v, ",") {
			if tok := strings.Trim(part, " \t"); tok != "" && !yield(tok) {
				return
			}
		}
	}
}
