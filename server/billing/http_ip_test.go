package billing

import (
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"

	chshare "github.com/openrport/openrport/share"
)

// Behind a trusted load balancer the client's address comes from X-Forwarded-For (webhook logs, audit actors).
func TestRemoteIPHonorsTrustedProxies(t *testing.T) {
	chshare.SetTrustedProxies([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	t.Cleanup(func() { chshare.SetTrustedProxies(nil) })
	r := httptest.NewRequest("POST", "/billing/webhook", nil)
	r.RemoteAddr = "10.1.2.3:4567"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	assert.Equal(t, "203.0.113.9", remoteIP(r))

	r.RemoteAddr = "[2001:db8::1]:443"
	assert.Equal(t, "2001:db8::1", remoteIP(r), "an untrusted peer's header is ignored")
}
