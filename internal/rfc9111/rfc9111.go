// Package rfc9111 is pure HTTP caching policy (RFC 9111, RFC 9110 §13, RFC
// 5861): header bytes in, decisions out. It performs no I/O and owns no clock;
// `now time.Time` is always a parameter so every rule is a table test.
//
// STUB written by the integrator to freeze the contract; workstream A owns this
// package and replaces the bodies, keeping every exported signature.
// StripHopByHop is implemented for real (fully specified by RFC 9110 §7.6.1).
package rfc9111

import (
	"net/http"
	"strings"
	"time"
)

// ReqDirectives is the parsed request Cache-Control (§5.2.1). Has* flags tell an
// absent directive from a zero value; MaxStaleAny is a bare "max-stale".
type ReqDirectives struct {
	NoStore, NoCache, OnlyIfCached, NoTransform bool
	MaxAge, MaxStale, MinFresh                  time.Duration
	HasMaxAge, HasMaxStale, MaxStaleAny         bool
	HasMinFresh                                 bool
}

// RespDirectives is the parsed response Cache-Control (§5.2.2 + RFC 5861).
// NoCacheFields / PrivateFields carry the qualified forms (no-cache="Set-Cookie").
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
type Policy struct {
	HeuristicFraction  float64
	HeuristicMax       time.Duration
	CacheAuthorization bool
}

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

// ParseRequest parses the request's Cache-Control (quoted-string aware) and
// Pragma: no-cache.
func ParseRequest(h http.Header) ReqDirectives {
	_ = h
	return ReqDirectives{}
}

// ParseResponse parses the response's Cache-Control, including qualified
// no-cache="..." / private="..." forms and RFC 5861 extensions.
func ParseResponse(h http.Header) RespDirectives {
	_ = h
	return RespDirectives{}
}

// Storable applies §3: cacheable method and status, no no-store on either side,
// no Vary: *, not private, explicit or heuristic freshness, and the §3.5
// Authorization rule unless p.CacheAuthorization. The Reason explains a false.
func (p Policy) Storable(r *http.Request, rd ReqDirectives, res Resp, sd RespDirectives) (bool, Reason) {
	_, _, _, _, _ = p, r, rd, res, sd
	return false, ReasonNotImplemented
}

// Lifetime computes the freshness lifetime (§4.2.1): s-maxage > max-age >
// Expires - Date > heuristic (§4.2.2). The bool reports a heuristic lifetime.
func (p Policy) Lifetime(res Resp, sd RespDirectives) (time.Duration, bool) {
	_, _, _ = p, res, sd
	return 0, false
}

// Evaluate decides how a stored response may be used at now: Fresh, Stale,
// Revalidate, StaleWhileRevalidate, Bypass or GatewayTimeout, honouring the
// request's max-age / max-stale / min-fresh / no-cache / only-if-cached and the
// response's must-revalidate / proxy-revalidate / swr / sie.
func (p Policy) Evaluate(now time.Time, res Resp, sd RespDirectives, rd ReqDirectives) Decision {
	_, _, _, _, _ = p, now, res, sd, rd
	return Decision{Action: Bypass, Reason: ReasonNotImplemented}
}

// Age computes the current age (§4.2.3) of res at now, including response_delay,
// clamped at 0.
func Age(res Resp, now time.Time) time.Duration {
	_, _ = res, now
	return 0
}

// ConditionalRequest adds If-None-Match (stored ETag) and/or If-Modified-Since
// (stored Last-Modified) to h so the origin can answer 304.
func ConditionalRequest(h http.Header, res Resp) {
	_, _ = h, res
}

// Merge304 applies §4.3.4: every field in fresh replaces the stored one; stored
// fields absent from fresh are kept, never dropped. Content-Length in a 304 is
// ignored. The result is a new Header.
func Merge304(stored, fresh http.Header) http.Header {
	_, _ = stored, fresh
	return http.Header{}
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
