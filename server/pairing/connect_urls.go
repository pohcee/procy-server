package pairing

import (
	"net"
	"net/url"
	"strings"
)

// PublicConnectURLs returns the client connect URLs ([server] url) that devices can reach, in
// order: URLs that do not parse or whose host is unspecified (0.0.0.0, ::) or loopback are
// dropped, so the default "http://0.0.0.0:80" is never sent to clients.
func PublicConnectURLs(urls []string) []string {
	var out []string
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			continue
		}
		host := strings.ToLower(u.Hostname())
		if host == "localhost" {
			continue
		}
		if ip := net.ParseIP(host); ip != nil && (ip.IsUnspecified() || ip.IsLoopback()) {
			continue
		}
		out = append(out, raw)
	}
	return out
}
