package chshare

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
)

// trustedProxies holds the reverse proxies whose X-Forwarded-For header is believed.
// Empty by default: a direct client must not be able to choose its own IP address.
var trustedProxies atomic.Pointer[[]netip.Prefix]

// SetTrustedProxies sets the reverse proxies used by RemoteIP. Call it once at startup.
func SetTrustedProxies(prefixes []netip.Prefix) {
	trustedProxies.Store(&prefixes)
}

// ParseTrustedProxies parses IP addresses and CIDR ranges of trusted reverse proxies.
func ParseTrustedProxies(list []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range list {
		s = strings.TrimSpace(s)
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy %q: expected an IP address or CIDR range", s)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// RemoteIP returns the client's IP address, honouring X-Forwarded-For only from trusted proxies.
func RemoteIP(r *http.Request) string {
	var trusted []netip.Prefix
	if p := trustedProxies.Load(); p != nil {
		trusted = *p
	}
	return ClientIP(r, trusted)
}

// ClientIP returns the client's IP address. X-Forwarded-For is used only when the direct peer is a
// trusted proxy; the header is then read from the right, skipping trusted proxies, because a client
// can write anything at its left end but not at its right.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	peerAddr, err := netip.ParseAddr(peer)
	if err != nil || !isTrusted(peerAddr, trusted) {
		return peer
	}

	var hops []netip.Addr
	for _, h := range strings.Split(r.Header.Get("X-Forwarded-For"), ",") {
		if a, err := netip.ParseAddr(strings.TrimSpace(h)); err == nil {
			hops = append(hops, a.Unmap())
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		if !isTrusted(hops[i], trusted) {
			return hops[i].String()
		}
	}
	if len(hops) > 0 {
		return hops[0].String() // every hop is a trusted proxy: the left-most is the origin
	}
	return peer
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	a = a.Unmap()
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
