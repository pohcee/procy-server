package main

import (
	"context"
	"fmt"
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

// e2eClient is one running rport client process.
type e2eClient struct {
	t   *testing.T
	cmd *exec.Cmd
	log *syncBuffer
}

// startClient starts an rport client (id, credentials) against the server's client port; it is stopped with the
// test.
func (s *e2eServer) startClient(id, authID, authPass string) *e2eClient {
	s.t.Helper()
	_, rport := e2eBinaries(s.t)
	dir := filepath.Join(s.root, "client-"+id)
	require.NoError(s.t, os.MkdirAll(dir, 0o700))
	conf := fmt.Sprintf(`[client]
  server = "127.0.0.1:%d"
  auth = "%s:%s"
  id = %q
  name = %q
  data_dir = %q
[connection]
  max_retry_interval = "1s"
[remote-commands]
  enabled = true
  allow = ['^/usr/bin/.*', '^/bin/.*', '^echo .*']
[remote-scripts]
  enabled = false
[logging]
  log_level = "info"
`, s.clientPort, authID, authPass, id, id, dir)
	cp := filepath.Join(dir, "rport.conf")
	require.NoError(s.t, os.WriteFile(cp, []byte(conf), 0o600))
	c := &e2eClient{t: s.t, cmd: exec.Command(rport, "-c", cp), log: &syncBuffer{}}
	c.cmd.Dir = dir
	c.cmd.Stdout = c.log
	c.cmd.Stderr = c.log
	require.NoError(s.t, c.cmd.Start())
	s.t.Cleanup(c.stop)
	return c
}

func (c *e2eClient) stop() {
	if c.cmd.ProcessState == nil {
		_ = c.cmd.Process.Signal(syscall.SIGTERM)
		_ = c.cmd.Wait()
	}
}

// as is the server seen by another API user (same process).
func (s *e2eServer) as(user, pass string) *e2eServer {
	c := *s
	c.user, c.pass, c.clients = user, pass, nil
	return &c
}

// waitConnected waits until the API user sees the client connected.
func (s *e2eServer) waitConnected(id string, c *e2eClient) {
	s.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		code, body := s.rawAPI(http.MethodGet, "/clients/"+id, nil)
		if code == http.StatusOK && strings.Contains(body, `"connection_state":"connected"`) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.t.Fatalf("client %s did not connect:\n%s\nserver:\n%s", id, c.log.String(), s.log.String())
}

// waitLog waits until the client logged want.
func (c *e2eClient) waitLog(want string, server *e2eServer) {
	c.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(c.log.String(), want) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	c.t.Fatalf("client did not log %q:\n%s\nserver:\n%s", want, c.log.String(), server.log.String())
}

func (s *e2eServer) clientIDs() []string {
	s.t.Helper()
	var list []map[string]any
	s.mustAPI(http.MethodGet, "/clients", nil, &list)
	var ids []string
	for _, c := range list {
		ids = append(ids, fmt.Sprint(c["id"]))
	}
	return ids
}

func (s *e2eServer) runCommand(clientID, command string) map[string]any {
	s.t.Helper()
	var started struct {
		JID string `json:"jid"`
	}
	s.mustAPI(http.MethodPost, "/clients/"+clientID+"/commands", map[string]any{"command": command, "timeout_sec": 10}, &started)
	return s.waitJob(clientID, started.JID)
}

// TestPostgresNodeDevicesOfEveryTenant runs one node on Postgres with two tenants: a real client of each tenant
// connects, each tenant sees and commands only its own client, an auth id can't be taken twice, the client limit of
// a tenant refuses a new client but not a known one reconnecting, and a suspended tenant's client is refused.
func TestPostgresNodeDevicesOfEveryTenant(t *testing.T) {
	d := pgtest.NewEmpty(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	keyFile := filepath.Join(root, "vault.key")
	require.NoError(t, os.WriteFile(keyFile, []byte(strings.Repeat("ab", 32)+"\n"), 0o600))
	configPath, clientPort, apiURL := e2eConfig(t, root, dataDir, "", "", fmt.Sprintf(`
[storage]
  backend = "postgres"
  dsn = %q
  role = "all"

[vault]
  key_provider = "local"
  local_key_file = %q
`, d.AppURL, keyFile))
	runRportd(t, "", "migrate", "-c", configPath, "--dsn", d.OwnerURL, "--grant-to", pgtest.AppRole)

	ctx := context.Background()
	app, err := postgres.Open(ctx, d.AppURL)
	require.NoError(t, err)
	defer app.Close()
	const pass = "tenant-user-password-0"
	for _, tc := range []struct {
		id   tenant.ID
		user string
	}{{"a", "alice"}, {"b", "bob"}} {
		require.NoError(t, pgserver.AddTenant(ctx, d.Owner, tc.id, ""))
		require.NoError(t, pgserver.AddUser(ctx, app, tc.id, tc.user, pass, []string{"Administrators"}))
	}

	alice := startE2EServer(t, root, configPath, clientPort, apiURL, "alice", pass)
	bob := alice.as("bob", pass)

	// credentials per tenant; an id is taken once across the tenants
	alice.mustAPI(http.MethodPost, "/clients-auth", map[string]string{"id": "auth-a", "password": "client-password-a"}, nil)
	bob.mustAPI(http.MethodPost, "/clients-auth", map[string]string{"id": "auth-b", "password": "client-password-b"}, nil)
	bob.mustAPI(http.MethodPost, "/clients-auth", map[string]string{"id": "auth-b2", "password": "client-password-b"}, nil)
	code, body := bob.rawAPI(http.MethodPost, "/clients-auth", map[string]string{"id": "auth-a", "password": "client-password-x"})
	assert.Equal(t, http.StatusConflict, code, body)

	ca := alice.startClient("client-a", "auth-a", "client-password-a")
	cb := bob.startClient("client-b", "auth-b", "client-password-b")
	alice.waitConnected("client-a", ca)
	bob.waitConnected("client-b", cb)

	assert.Equal(t, []string{"client-a"}, alice.clientIDs())
	assert.Equal(t, []string{"client-b"}, bob.clientIDs())
	code, _ = alice.rawAPI(http.MethodGet, "/clients/client-b", nil)
	assert.Equal(t, http.StatusNotFound, code, "another tenant's client")

	for _, tc := range []struct {
		s  *e2eServer
		id string
	}{{alice, "client-a"}, {bob, "client-b"}} {
		job := tc.s.runCommand(tc.id, "echo hello-from-"+tc.id)
		require.Equal(t, "successful", job["status"], "%v", job)
		assert.Contains(t, fmt.Sprint(job["result"]), "hello-from-"+tc.id)
		var jobs []map[string]any
		tc.s.mustAPI(http.MethodGet, "/clients/"+tc.id+"/commands", nil, &jobs)
		assert.Len(t, jobs, 1, "the job result is stored in the client's tenant")
	}
	code, _ = alice.rawAPI(http.MethodPost, "/clients/client-b/commands", map[string]any{"command": "echo no", "timeout_sec": 5})
	assert.Equal(t, http.StatusNotFound, code)

	// a tunnel to tenant b's client is kept with that client
	bob.mustAPI(http.MethodPut, "/clients/client-b/tunnels?remote=127.0.0.1:22&check_port=0", nil, nil)
	var clientB map[string]any
	bob.mustAPI(http.MethodGet, "/clients/client-b", nil, &clientB)
	assert.Len(t, clientB["tunnels"], 1, "%v", clientB)

	// the client limit: b may have 1 client; a new one is refused, the known one reconnects
	_, err = d.Owner.Exec("UPDATE tenants SET max_clients = 1 WHERE id = 'b'")
	require.NoError(t, err)
	cb2 := bob.startClient("client-b2", "auth-b2", "client-password-b")
	cb2.waitLog("the organization's limit of 1 clients is reached", alice)
	assert.Contains(t, alice.log.String(), `refused new client client-b2 (client auth id "auth-b2"): tenant "b" has 1 of its 1 clients`)
	assert.Equal(t, []string{"client-b"}, bob.clientIDs())
	cb2.stop()
	cb.stop()
	cb = bob.startClient("client-b", "auth-b", "client-password-b")
	bob.waitConnected("client-b", cb)

	// a suspended tenant's client is refused when it connects
	_, err = d.Owner.Exec("UPDATE tenants SET status = 'suspended' WHERE id = 'a'")
	require.NoError(t, err)
	ca.stop()
	ca = alice.startClient("client-a", "auth-a", "client-password-a")
	ca.waitLog("the organization of this client is suspended", alice)
	assert.Contains(t, alice.log.String(), `refused client client-a (client auth id "auth-a"): tenant "a" is suspended`)
	t.Logf("client a refused: %s", lastLine(ca.log.String(), "suspended"))
}

func lastLine(log, containing string) string {
	lines := strings.Split(log, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], containing) {
			return lines[i]
		}
	}
	return ""
}
