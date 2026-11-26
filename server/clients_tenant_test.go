package chserver

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/pohcee/procy-server/server/chconfig"
	"github.com/pohcee/procy-server/server/clients"
	"github.com/pohcee/procy-server/server/clientsauth"
	"github.com/pohcee/procy-server/share/comm"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/models"
	"github.com/pohcee/procy-server/share/security"
	"github.com/pohcee/procy-server/share/tenant"
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
		server: &listenerServices{
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

func TestClientRequestsAreSavedInClientTenant(t *testing.T) {
	p := clients.NewFakeClientProvider(t, nil)
	defer p.Close()
	repo := clients.NewClientRepositoryWithDB(nil, nil, p, testLog)
	a, b := tenant.WithID(context.Background(), "a"), tenant.WithID(context.Background(), "b")
	for _, ctx := range []context.Context{a, b} {
		c := clients.New(t).ID("c1").Logger(testLog).Build()
		c.Context = ctx
		require.NoError(t, repo.Save(ctx, c))
	}
	cl := &ClientListener{
		logger: testLog,
		server: &listenerServices{
			config:        &chconfig.Config{Server: chconfig.ServerConfig{MaxRequestBytesClient: 1024 * 1024}},
			clientService: clients.NewClientService(nil, nil, repo, testLog, nil),
		},
	}

	send := func(ctx context.Context) {
		payload, err := json.Marshal(&models.UpdatesStatus{UpdatesAvailable: 3})
		require.NoError(t, err)
		reqs := make(chan *ssh.Request, 1)
		reqs <- &ssh.Request{Type: comm.RequestTypeUpdatesStatus, Payload: payload}
		close(reqs)
		cl.handleSSHRequests(ctx, logger.ForkToDynamicLogger(testLog, "client", false, false), "c1", reqs)
	}

	// without a tenant nothing is saved
	send(context.Background())
	got, err := repo.GetByID(a, "c1")
	require.NoError(t, err)
	assert.Nil(t, got.UpdatesStatus)

	// the client's context decides the tenant
	send(a)
	got, err = repo.GetByID(a, "c1")
	require.NoError(t, err)
	require.NotNil(t, got.UpdatesStatus)
	assert.Equal(t, 3, got.GetUpdatesStatus().UpdatesAvailable)
	other, err := repo.GetByID(b, "c1")
	require.NoError(t, err)
	assert.Nil(t, other.UpdatesStatus)
}
