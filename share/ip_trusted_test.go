package chshare

import (
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientIPIgnoresForwardedForFromUntrustedPeer(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "203.0.113.7:4321"
	req.Header.Set("X-Forwarded-For", "8.8.8.8")

	assert.Equal(t, "203.0.113.7", ClientIP(req, nil), "a direct client must not choose its own IP")
}

func TestClientIPUsesForwardedForFromTrustedProxy(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.1.2.3:4321"
	req.Header.Set("X-Forwarded-For", "8.8.8.8")

	assert.Equal(t, "8.8.8.8", ClientIP(req, trusted))
}

func TestClientIPTakesRightmostUntrustedAddress(t *testing.T) {
	// The client prepended a fake address; the proxy appended the real one.
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.1.2.3:4321"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 198.51.100.9, 10.9.9.9")

	assert.Equal(t, "198.51.100.9", ClientIP(req, trusted))
}

func TestParseTrustedProxies(t *testing.T) {
	got, err := ParseTrustedProxies([]string{"10.0.0.0/8", "192.0.2.1"})
	require.NoError(t, err)
	assert.Equal(t, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.0.2.1/32")}, got)

	_, err = ParseTrustedProxies([]string{"not-an-ip"})
	assert.Error(t, err)
}
