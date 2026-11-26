package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/storage/postgres"
	"github.com/pohcee/procy-server/server/storage/postgres/pgserver"
	"github.com/pohcee/procy-server/server/storage/postgres/pgtest"
)

// TestPostgresDeviceProcessesShareTheClientLimit runs an api process and two device processes on one database, as
// two nodes of a cluster do. The client limit of an organization holds across the device processes (a new client on
// the second one is refused while the first one holds the organization's only place), and a client the organization
// has already is admitted by any device process, also when the organization is at or over its limit (failover after
// the device process holding it stopped).
func TestPostgresDeviceProcessesShareTheClientLimit(t *testing.T) {
	d := pgtest.NewEmpty(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	keyFile := filepath.Join(root, "vault.key")
	require.NoError(t, os.WriteFile(keyFile, []byte(strings.Repeat("cd", 32)+"\n"), 0o600))
	apiConf, _, apiURL := e2eConfig(t, root, dataDir, "", "", fmt.Sprintf(`
[storage]
  backend = "postgres"
  dsn = %q
  role = "api"

[vault]
  key_provider = "local"
  local_key_file = %q
`, d.AppURL, keyFile))
	runRportd(t, "", "migrate", "-c", apiConf, "--dsn", d.OwnerURL, "--grant-to", pgtest.AppRole, "--grant-device-to", pgtest.DeviceRole)

	ctx := context.Background()
	app, err := postgres.Open(ctx, d.AppURL)
	require.NoError(t, err)
	defer app.Close()
	const pass = "tenant-user-password-0"
	require.NoError(t, pgserver.AddTenant(ctx, d.Owner, "b", ""))
	require.NoError(t, pgserver.AddUser(ctx, app, "b", "bob", pass, []string{"Administrators"}))
	api := startE2EServer(t, root, apiConf, 0, apiURL, "bob", pass)
	var cred struct {
		Password string `json:"password"`
	}
	api.mustAPI(http.MethodPost, "/clients-auth", map[string]string{"id": "b-auth"}, &cred)
	require.NotEmpty(t, cred.Password)
	setLimit := func(n int) {
		_, err := d.Owner.Exec("UPDATE tenants SET max_clients = $1 WHERE id = 'b'", n)
		require.NoError(t, err)
	}

	// two device processes, each seen through the api process by bob
	device := func(node string) (*e2eServer, func()) {
		dir := filepath.Join(root, node)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		conf, port := deviceConfigNode(t, dir, d.DeviceURL, node)
		log, stop := startDeviceProcess(t, dir, conf, port)
		return &e2eServer{t: t, root: dir, clientPort: port, apiURL: apiURL, user: "bob", pass: pass, log: log}, stop
	}
	dev1, stopDev1 := device("device-1")
	dev2, _ := device("device-2")

	// the organization's only place is taken on device-1: a new client on device-2 is refused
	setLimit(1)
	b1 := dev1.startClient("b1", "b-auth", cred.Password)
	api.waitConnected("b1", b1)
	b3 := dev2.startClient("b3", "b-auth", cred.Password)
	b3.waitLog("the organization's limit of 1 clients is reached", dev2)
	assert.Contains(t, dev2.log.String(), `refused new client b3 (client auth id "b-auth"): tenant "b" has 1 of its 1 clients`)
	assert.Equal(t, []string{"b1"}, api.clientIDs())
	b3.stop()

	// with a second place b3 connects to device-2
	setLimit(2)
	b3 = dev2.startClient("b3", "b-auth", cred.Password)
	api.waitConnected("b3", b3)

	// the limit lowered below what the organization has; device-1 stops and b1 connects to device-2, which has
	// never held it: an existing device is admitted
	setLimit(1)
	stopDev1()
	b1.stop()
	b1 = dev2.startClient("b1", "b-auth", cred.Password)
	api.waitConnected("b1", b1)
	assert.NotContains(t, dev2.log.String(), "refused new client b1")

	// a new client is still refused
	b4 := dev2.startClient("b4", "b-auth", cred.Password)
	b4.waitLog("the organization's limit of 1 clients is reached", dev2)
	assert.ElementsMatch(t, []string{"b1", "b3"}, api.clientIDs())

	// a device process runs none of the api processes' background work
	for _, task := range []string{"Task to cleanup jobs", "Task to cleanup expired api sessions", "Task to purge disconnected clients"} {
		assert.NotContains(t, dev2.log.String(), task)
	}
	assert.Contains(t, api.log.String(), "Task to cleanup jobs")
	assert.NotContains(t, dev2.log.String(), "nil request")
}
