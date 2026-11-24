package chserver

import (
	"net"
	"strings"
)

// clientHostname is the hostname a client connected with, taken from the Host header of its
// websocket request: lowercase, without port or trailing dot. Rollovers to a new server address
// use it to see which clients still use the old name.
func clientHostname(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return strings.TrimSuffix(strings.ToLower(host), ".")
}
