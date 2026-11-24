package chserver

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"golang.org/x/crypto/ssh"

	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/clientsauth"
	"github.com/openrport/openrport/share/security"
	"github.com/openrport/openrport/share/tenant"
)

// clientsTestCtx is the context of standalone server work: it carries the default tenant.
var clientsTestCtx = tenant.WithID(context.Background(), tenant.Default)

type testConnMetadata struct {
	ssh.ConnMetadata
	user string
}

func (m testConnMetadata) User() string { return m.user }

func (m testConnMetadata) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 1234}
}

func TestAuthUserUsesListenerContextTenant(t *testing.T) {
	cl := &ClientListener{
		logger:            testLog,
		bannedClientAuths: security.NewBanList(time.Second),
		server: &Server{
			config:             &chconfig.Config{},
			clientAuthProvider: clientsauth.NewSingleProvider("auth1", "secret"),
		},
	}
	conn := testConnMetadata{user: "auth1"}

	cl.ctx = clientsTestCtx
	_, err := cl.authUser(conn, []byte("secret"))
	assert.NoError(t, err)

	// the listener context is the only tenant source: without one the credentials are not looked up
	cl.ctx = context.Background()
	_, err = cl.authUser(conn, []byte("secret"))
	assert.ErrorIs(t, err, tenant.ErrMissing)
}
