package clients

import (
	"net"
	"testing"

	mapset "github.com/deckarep/golang-set"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/clients/clientdata"
	"github.com/pohcee/procy-server/server/ports"
	chshare "github.com/pohcee/procy-server/share"
	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/test"
)

func TestStartClientRecordsConnectedHost(t *testing.T) {
	cs := &ClientServiceProvider{
		repo:            NewClientRepository(nil, nil, testLog),
		portDistributor: ports.NewPortDistributor(mapset.NewSet()),
		logger:          testLog,
	}
	start := func(host string) *clientdata.Client {
		conn := test.NewConnMock()
		conn.ReturnRemoteAddr = &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 2345}
		c, err := cs.StartClient(defaultCtx, "auth", "c1", conn, host, false,
			&chshare.ConnectionRequest{Name: "c1", Version: "0.9.31"}, testLog)
		require.NoError(t, err)
		return c
	}
	c := start("connect.procy.sh")
	assert.Equal(t, "connect.procy.sh", c.ConnectedHost)
	c.SetDisconnectedNow()
	c = start("us-central1.connect.procy.sh")
	assert.Equal(t, "us-central1.connect.procy.sh", c.ConnectedHost, "a reconnect overwrites the host")
}

func TestConnectedHostIsStored(t *testing.T) {
	ctx := defaultCtx
	p := NewFakeClientProvider(t, nil)
	defer p.Close()
	c := New(t).Logger(testLog).Build()
	c.ConnectedHost = "connect.procy.sh"
	require.NoError(t, p.Save(ctx, c))
	got, err := p.get(ctx, c.GetID(), testLog)
	require.NoError(t, err)
	assert.Equal(t, "connect.procy.sh", got.ConnectedHost)
}

func TestConnectedHostInPayload(t *testing.T) {
	c := &clientdata.Client{ID: "c1", ConnectedHost: "connect.procy.sh"}
	cc := clientdata.NewCalculatedClient(c, nil, clientdata.Connected)
	p := ConvertToClientPayload(cc, []query.FieldsOption{{Resource: "clients", Fields: []string{"connected_host"}}})
	require.NotNil(t, p.ConnectedHost)
	assert.Equal(t, "connect.procy.sh", *p.ConnectedHost)
	assert.True(t, OptionsSupportedFilters["connected_host"])
}

func TestFilterByConnectedHost(t *testing.T) {
	old := &clientdata.Client{ID: "old", ConnectedHost: "rport.example.com", Logger: testLog}
	moved := &clientdata.Client{ID: "moved", ConnectedHost: "connect.example.com", Logger: testLog}
	repo := NewClientRepository([]*clientdata.Client{old, moved}, nil, testLog)
	got, err := repo.GetFilteredUserClients(defaultCtx, admin, []query.FilterOption{{Column: []string{"connected_host"}, Values: []string{"rport.example.com"}}}, nil)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "old", got[0].ID)
}
