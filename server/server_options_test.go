package chserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/share/files"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/tenant"
)

// fixedResolver resolves every request to one tenant and counts the calls.
type fixedResolver struct {
	id    tenant.ID
	calls chan struct{}
}

func (r *fixedResolver) Resolve(*http.Request) (tenant.ID, error) {
	select {
	case r.calls <- struct{}{}:
	default:
	}
	return r.id, nil
}

func TestNewServerUsesTenantResolver(t *testing.T) {
	res := &fixedResolver{id: tenant.Default, calls: make(chan struct{}, 1)}
	s, err := NewServer(context.Background(), storageTestConfig(t), &ServerOpts{FilesAPI: files.NewFileSystem(), TenantResolver: res})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	assert.Same(t, res, s.apiListener.tenantResolver)

	s.apiListener.router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	select {
	case <-res.calls:
	default:
		t.Fatal("the API router did not ask the resolver")
	}

	local, err := NewServer(context.Background(), storageTestConfig(t), &ServerOpts{FilesAPI: files.NewFileSystem()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = local.Close() })
	assert.Nil(t, local.apiListener.tenantResolver, "nil means the default resolver")
}

// With DisableClientListener the server runs the API only: no client listener is built or opened,
// and the API answers.
func TestNewServerWithoutClientListener(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("counts listening sockets through /proc")
	}
	config := storageTestConfig(t)
	config.API.Address = freeAddr(t)
	config.API.MaxRequestBytes = 1 << 20
	config.API.Auth = "admin:a-long-api-password"
	config.Server.ListenAddress = freeAddr(t)
	config.Server.MaxRequestBytesClient = 1 << 20
	config.Server.MaxConcurrentSSHConnectionHandshakes = 4
	config.Server.CheckClientsConnectionInterval = time.Minute
	config.Server.CheckClientsConnectionTimeout = 10 * time.Second
	config.Server.RunRemoteCmdTimeoutSec = 60
	config.Server.URL = []string{"http://" + config.Server.ListenAddress}
	config.Server.UsedPortsRaw = []string{"20000-30000"}
	config.Server.Auth = "client:a-long-client-secret"
	mLog := logger.NewMemLogger()
	require.NoError(t, config.ParseAndValidate(&mLog))

	s, err := NewServer(context.Background(), config, &ServerOpts{FilesAPI: files.NewFileSystem(), DisableClientListener: true})
	require.NoError(t, err)
	assert.Nil(t, s.clientListener)
	s.HandlePlusLicenseInfoAvailable() // nil-safe

	before := listeningSockets(t)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-runErr:
			if err != nil {
				t.Logf("Run returned: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after the context was canceled")
		}
	}()

	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + config.API.Address + "/api/v1/status")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusUnauthorized
	}, 5*time.Second, 20*time.Millisecond, "the API answers")
	time.Sleep(200 * time.Millisecond) // let every listener that would start do so
	assert.Equal(t, before+1, listeningSockets(t), "only the API listener listens")

	_, err = http.Get("http://" + config.Server.ListenAddress + "/")
	assert.Error(t, err, "nothing listens on the client address")
}
