package chserver

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	chclient "github.com/openrport/openrport/client"
	"github.com/openrport/openrport/share/clientconfig"
	"github.com/openrport/openrport/share/files"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
	"github.com/openrport/openrport/share/test"
)

// listeningSockets counts the TCP sockets of this process that are in the LISTEN state.
func listeningSockets(t *testing.T) int {
	t.Helper()
	inodes := map[string]bool{}
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(table)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			f := strings.Fields(line)
			if len(f) > 9 && f[3] == "0A" { // TCP_LISTEN
				inodes[f[9]] = true
			}
		}
	}
	fds, err := os.ReadDir("/proc/self/fd")
	require.NoError(t, err)
	n := 0
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		if err != nil || !strings.HasPrefix(target, "socket:[") {
			continue
		}
		if inodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] {
			n++
		}
	}
	return n
}

// With [api] address = "" the server runs the client listener only: no API or web UI listener is
// opened, and clients still connect.
func TestEmptyAPIAddressRunsClientListenerOnly(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("counts listening sockets through /proc")
	}
	config := storageTestConfig(t)
	config.API.Address = ""
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

	s, err := NewServer(context.Background(), config, &ServerOpts{FilesAPI: files.NewFileSystem()})
	require.NoError(t, err)

	before := listeningSockets(t)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after the context was cancelled")
		}
	}()

	require.Eventually(t, func() bool { return listeningSockets(t) > before }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond) // let every listener that would start do so
	assert.Equal(t, before+1, listeningSockets(t), "only the client listener listens")

	// a client connects
	clientCfg := &chclient.ClientConfigHolder{Config: &clientconfig.Config{
		Client: clientconfig.ClientConfig{
			Server:  "http://" + config.Server.ListenAddress,
			Auth:    "client:a-long-client-secret",
			DataDir: t.TempDir(),
		},
		RemoteCommands: clientconfig.CommandsConfig{Order: [2]string{"allow", "deny"}},
		Logging:        clientconfig.LogConfig{LogLevel: logger.LogLevelError, LogOutput: logger.LogOutput{File: os.Stdout}},
		Connection:     clientconfig.ConnectionConfig{MaxRetryCount: -1},
	}}
	require.NoError(t, clientCfg.ParseAndValidate(true))
	c, err := chclient.NewClient(clientCfg, test.NewFileAPIMock())
	require.NoError(t, err)
	clientCtx, stopClient := context.WithCancel(context.Background())
	defer stopClient()
	require.NoError(t, c.Start(clientCtx))
	defer c.Close()

	serverCtx := tenant.WithID(context.Background(), tenant.Default)
	require.Eventually(t, func() bool { return s.clientService.CountActive(serverCtx) == 1 }, 20*time.Second, 50*time.Millisecond,
		"the client connects to the client listener")
}
