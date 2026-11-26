package chserver

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/chconfig"
	"github.com/pohcee/procy-server/server/clients"
	"github.com/pohcee/procy-server/server/clientsauth"
	"github.com/pohcee/procy-server/server/dispatch"
	"github.com/pohcee/procy-server/server/jobrunner"
	"github.com/pohcee/procy-server/share/models"
	"github.com/pohcee/procy-server/share/tenant"
)

// freeAddr returns a local address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func listenerTestOptions(t *testing.T) ClientListenerOptions {
	t.Helper()
	key, err := initPrivateKey("test-seed")
	require.NoError(t, err)
	cfg := &chconfig.Config{Server: chconfig.ServerConfig{
		MaxRequestBytesClient:                1 << 20,
		MaxConcurrentSSHConnectionHandshakes: 1,
	}}
	cfg.Logging.LogLevel = testLog.Level
	return ClientListenerOptions{
		Config:        cfg,
		PrivateKey:    key,
		ClientService: clients.NewClientService(nil, nil, clients.NewClientRepository(nil, nil, testLog), testLog, nil),
		ClientAuth:    clientsauth.NewSingleProvider("client", "secret"),
		Jobs:          NewJobProviderMock(),
		JobsDone:      jobrunner.NewDoneChannels(),
		UIJobStreams:  newUIStreams(),
		FileGrants:    dispatch.NewFileGrants(),
		Measurements:  &recordingMeasurementQueue{},
		Capabilities:  &models.Capabilities{},
	}
}

// The client listener is built from its options alone: no Server, API listener or job runner.
func TestNewClientListenerFromOptions(t *testing.T) {
	cl, err := NewClientListener(listenerTestOptions(t))
	require.NoError(t, err)

	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(tenant.WithID(context.Background(), tenant.Default))
	defer cancel()
	require.NoError(t, cl.Start(ctx, addr))

	var resp *http.Response
	require.Eventually(t, func() bool {
		resp, err = http.Get("http://" + addr + "/")
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "a plain request is not a client")

	require.NoError(t, cl.Close())
}

func TestNewClientListenerRequiresOptions(t *testing.T) {
	for field, clear := range map[string]func(*ClientListenerOptions){
		"Config":        func(o *ClientListenerOptions) { o.Config = nil },
		"PrivateKey":    func(o *ClientListenerOptions) { o.PrivateKey = nil },
		"ClientService": func(o *ClientListenerOptions) { o.ClientService = nil },
		"ClientAuth":    func(o *ClientListenerOptions) { o.ClientAuth = nil },
		"Jobs":          func(o *ClientListenerOptions) { o.Jobs = nil },
		"JobsDone":      func(o *ClientListenerOptions) { o.JobsDone = nil },
		"UIJobStreams":  func(o *ClientListenerOptions) { o.UIJobStreams = nil },
		"FileGrants":    func(o *ClientListenerOptions) { o.FileGrants = nil },
		"Measurements":  func(o *ClientListenerOptions) { o.Measurements = nil },
		"Capabilities":  func(o *ClientListenerOptions) { o.Capabilities = nil },
	} {
		t.Run(field, func(t *testing.T) {
			o := listenerTestOptions(t)
			clear(&o)
			_, err := NewClientListener(o)
			assert.ErrorContains(t, err, field+" is required")
		})
	}
}
