// Package trustproxy decides whether a peer may speak for the client through
// X-Forwarded-Proto / X-Forwarded-For / X-Forwarded-Host / Forwarded. Those
// fields are plain request headers any client can set, so they are honoured
// only from peers inside a configured CIDR list (EDGECACHE_TRUSTED_PROXIES);
// the empty list trusts nobody. Without this a client could send
// "X-Forwarded-Proto: https" over plain HTTP and plant an entry under the
// https cache key (RFC 9111 §2: the key includes the scheme).
package trustproxy

import (
	"fmt"
	"net/netip"
	"strings"
)

// List is an immutable set of trusted prefixes. The zero value trusts nobody.
type List struct {
	prefixes []netip.Prefix
}

// New returns a List over prefixes (masked to their network address).
func New(prefixes []netip.Prefix) List {
	out := make([]netip.Prefix, 0, len(prefixes))
	for _, p := range prefixes {
		if p.IsValid() {
			out = append(out, p.Masked())
		}
	}
	return List{prefixes: out}
}

// Parse reads a comma-separated list of CIDRs or bare IPs ("10.0.0.0/8,
// 127.0.0.1, ::1"); a bare IP is a /32 or /128. Blank elements are skipped,
// so "" is the empty list.
func Parse(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			p, err := netip.ParsePrefix(part)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", part, err)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", part, err)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// Empty reports whether the list trusts nobody.
func (l List) Empty() bool { return len(l.prefixes) == 0 }

// Trusts reports whether remoteAddr (an http.Request.RemoteAddr, "ip:port" or
// a bare IP) is inside a trusted prefix. It does not allocate. An IPv4-mapped
// IPv6 peer is matched as IPv4; an unparseable address is never trusted.
func (l List) Trusts(remoteAddr string) bool {
	if len(l.prefixes) == 0 {
		return false
	}
	a, ok := peerAddr(remoteAddr)
	if !ok {
		return false
	}
	for _, p := range l.prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// String lists the prefixes, comma separated (config summaries).
func (l List) String() string {
	parts := make([]string, len(l.prefixes))
	for i, p := range l.prefixes {
		parts[i] = p.String()
	}
	return strings.Join(parts, ",")
}

func peerAddr(remoteAddr string) (netip.Addr, bool) {
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		return ap.Addr().Unmap(), true
	}
	if a, err := netip.ParseAddr(remoteAddr); err == nil {
		return a.Unmap(), true
	}
	return netip.Addr{}, false
}
