package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/pgserver"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	"github.com/openrport/openrport/share/tenant"
)

// deviceConfig writes the rportd.conf of a device process (client listener only, device role, no API, no vault).
func deviceConfig(t *testing.T, root, dsn string) (configPath string, clientPort int) {
	t.Helper()
	clientPort = e2eFreePort(t)
	dataDir := filepath.Join(root, "device-data")
	conf := fmt.Sprintf(`[server]
  address = "127.0.0.1:%d"
  url = "http://127.0.0.1:%d"
  key_seed = %q
  data_dir = %q
  allow_root = true

[logging]
  log_level = "info"

[api]
  address = ""

[storage]
  backend = "postgres"
  dsn = %q
  role = "device"

[cluster]
  node_id = "device-1"

[metering]
  enabled = true
  flush_interval = "5s"
`, clientPort, clientPort, e2eKeySeed, dataDir, dsn)
	configPath = filepath.Join(root, "rportd-device.conf")
	require.NoError(t, os.WriteFile(configPath, []byte(conf), 0o600))
	return configPath, clientPort
}

// startDeviceProcess starts rportd with a device config and waits until its client listener accepts connections.
func startDeviceProcess(t *testing.T, root, configPath string, clientPort int) (log *syncBuffer, stop func()) {
	t.Helper()
	rportd, _ := e2eBinaries(t)
	log = &syncBuffer{}
	cmd := exec.Command(rportd, "-c", configPath)
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = log, log
	require.NoError(t, cmd.Start())
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	stop = func() {
		select {
		case <-done:
			return
		default:
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("device process exited:\n%s", log.String())
		default:
		}
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", clientPort), 200*time.Millisecond); err == nil {
			_ = c.Close()
			return log, stop
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("device process did not listen:\n%s", log.String())
	return nil, nil
}

// TestPostgresDeviceProcessEndToEnd runs a node as two processes: an api process (role api, the application role)
// and a device process (role device, the device role). A credential created through the API (password shown once)
// lets a real client connect to the device process, which checks the argon2id hash; what the client reports reaches
// the database through the outbox and the ingest functions, and the API lists the client. The device process
// refuses to start with an over-privileged login.
func TestPostgresDeviceProcessEndToEnd(t *testing.T) {
	d := pgtest.NewEmpty(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	keyFile := filepath.Join(root, "vault.key")
	require.NoError(t, os.WriteFile(keyFile, []byte(hex.EncodeToString(key)+"\n"), 0o600))

	apiConf, _, apiURL := e2eConfig(t, root, dataDir, "", "", fmt.Sprintf(`
[storage]
  backend = "postgres"
  dsn = %q
  role = "api"

[vault]
  key_provider = "local"
  local_key_file = %q

[metering]
  enabled = true
`, d.AppURL, keyFile))
	out := runRportd(t, "", "migrate", "-c", apiConf, "--dsn", d.OwnerURL, "--grant-to", pgtest.AppRole, "--grant-device-to", pgtest.DeviceRole)
	assert.Contains(t, out, "granted the device privileges to "+pgtest.DeviceRole)
	app, err := postgres.Open(context.Background(), d.AppURL)
	require.NoError(t, err)
	defer app.Close()
	require.NoError(t, pgserver.AddUser(context.Background(), app, tenant.Default, e2eAdmin, e2eAdminPass, []string{"Administrators"}))

	api := startE2EServer(t, root, apiConf, 0, apiURL, e2eAdmin, e2eAdminPass)
	var created struct {
		ID       string `json:"id"`
		Password string `json:"password"`
	}
	api.mustAPI(http.MethodPost, "/clients-auth", map[string]string{"id": "dev-auth"}, &created)
	require.Equal(t, "dev-auth", created.ID)
	require.NotEmpty(t, created.Password, "the generated password is shown once")
	var listed []map[string]any
	api.mustAPI(http.MethodGet, "/clients-auth", nil, &listed)
	require.Len(t, listed, 1)
	assert.Empty(t, listed[0]["password"], "and never again")

	// an over-privileged login (the application role) is refused
	require.NoError(t, os.MkdirAll(filepath.Join(root, "bad"), 0o700))
	badConf, badPort := deviceConfig(t, filepath.Join(root, "bad"), d.AppURL)
	rportd, _ := e2eBinaries(t)
	badOut, err := exec.Command(rportd, "-c", badConf).CombinedOutput()
	require.Error(t, err, "rportd started on port %d:\n%s", badPort, badOut)
	assert.Contains(t, string(badOut), "more than the device role needs")

	devConf, devPort := deviceConfig(t, root, d.DeviceURL)
	devLog, stopDevice := startDeviceProcess(t, root, devConf, devPort)

	// a client on the device process; the API sees it once its state was shipped
	dev := &e2eServer{t: t, root: root, clientPort: devPort, apiURL: apiURL, user: e2eAdmin, pass: e2eAdminPass, log: devLog}
	t.Cleanup(dev.stop)
	dev.done = make(chan struct{})
	close(dev.done) // the process is stopped by stopDevice; dev.stop only stops the clients
	dev.connectClient("dev-client", "dev-auth", created.Password)

	var client map[string]any
	api.mustAPI(http.MethodGet, "/clients/dev-client", nil, &client)
	assert.Equal(t, "dev-auth", client["client_auth_id"])
	t.Logf("api process sees dev-client: connection_state=%v", client["connection_state"])

	owner := d.Owner
	var events int
	require.Eventually(t, func() bool {
		_ = owner.Get(&events, "SELECT count(*) FROM client_connection_events WHERE client_id = 'dev-client' AND event = 'connected' AND node_id = 'device-1'")
		return events == 1
	}, 20*time.Second, 200*time.Millisecond, "connection event shipped")

	// the device process meters through its outbox and the device role: the device's day and the connected count
	require.Eventually(t, func() bool {
		var days, peak int
		_ = owner.Get(&days, "SELECT count(*) FROM usage_device_days WHERE tenant_id = 'default' AND client_id = 'dev-client'")
		_ = owner.Get(&peak, "SELECT coalesce(max(value), 0) FROM usage_hourly WHERE tenant_id = 'default' AND metric = 'devices_peak'")
		return days == 1 && peak == 1
	}, 30*time.Second, 200*time.Millisecond, "usage shipped from the device process:\n%s", devLog.String())
	var report struct {
		Totals map[string]any `json:"totals"`
	}
	api.mustAPI(http.MethodGet, "/usage", nil, &report)
	assert.EqualValues(t, 1, report.Totals["device_days"], "the api process reports it")

	var outboxFiles []string
	outboxFiles, err = filepath.Glob(filepath.Join(root, "device-data", pgserver.OutboxFile))
	require.NoError(t, err)
	assert.Len(t, outboxFiles, 1)

	// stopping the device process: the client's disconnection reaches the database after the restart at the latest
	for _, c := range dev.clients {
		_ = c.Process.Signal(syscall.SIGTERM)
		_ = c.Wait()
	}
	dev.clients = nil
	require.Eventually(t, func() bool {
		var disconnected int
		_ = owner.Get(&disconnected, "SELECT count(*) FROM client_state WHERE client_id = 'dev-client' AND disconnected_at IS NOT NULL")
		return disconnected == 1
	}, 30*time.Second, 200*time.Millisecond, "disconnection shipped:\n%s", devLog.String())
	stopDevice()
	assert.NotContains(t, strings.ToLower(devLog.String()), "permission denied", devLog.String())
}
