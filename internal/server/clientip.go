package server

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

// ParseTrustedProxies reads FLEETLING_TRUSTED_PROXIES: a comma separated
// list of IPs or CIDRs whose X-Forwarded-For header is believed.
func ParseTrustedProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for f := range strings.SplitSeq(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if strings.Contains(f, "/") {
			p, err := netip.ParsePrefix(f)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", f, err)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", f, err)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

func trusted(a netip.Addr, list []netip.Prefix) bool {
	for _, p := range list {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// clientAddr is the address the login limiter keys on. A request from a
// trusted proxy is attributed to the right-most X-Forwarded-For entry that
// is not itself a trusted proxy. Entries further left are client-supplied
// and can be forged, so they are never used. Anything else uses the TCP
// peer address.
func clientAddr(r *http.Request, list []netip.Prefix) (netip.Addr, bool) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	peer := ap.Addr().Unmap()
	if !trusted(peer, list) {
		return peer, false
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		a = a.Unmap()
		if !trusted(a, list) {
			return a, true
		}
	}
	return peer, true
}

// isHTTPS decides whether cookies get the Secure flag. X-Forwarded-Proto
// only counts from a trusted proxy.
func isHTTPS(r *http.Request, list []netip.Prefix) bool {
	if r.TLS != nil {
		return true
	}
	if _, viaProxy := clientAddr(r, list); viaProxy {
		return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	}
	return false
}
