package main

// End to end on Postgres: builds rportd and the rport client from this module, starts rportd with
// [storage] backend = "postgres" (and the local vault key provider) on a fresh database, connects a real client and
// drives the API. Runs only with RPORT_TEST_POSTGRES_URL (see pgtest) and not with -short. Everything listens on
// 127.0.0.1 and lives in t.TempDir().

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/storage/postgres"
	"github.com/pohcee/procy-server/server/storage/postgres/pgserver"
	"github.com/pohcee/procy-server/server/storage/postgres/pgtest"
	"github.com/pohcee/procy-server/share/tenant"
)

var (
	e2eBuildOnce sync.Once
	e2eBinDir    string
	e2eBuildErr  error
)

// e2eBinaries builds rportd and rport once per test binary run.
func e2eBinaries(t *testing.T) (rportd, rport string) {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and runs rportd and rport; skipped with -short")
	}
	e2eBuildOnce.Do(func() {
		e2eBinDir, e2eBuildErr = os.MkdirTemp("", "rportd-e2e-bin-")
		if e2eBuildErr != nil {
			return
		}
		for _, b := range []string{"rportd", "rport"} {
			cmd := exec.Command("go", "build", "-o", filepath.Join(e2eBinDir, b), "./cmd/"+b)
			cmd.Dir = filepath.Join("..", "..")
			if out, err := cmd.CombinedOutput(); err != nil {
				e2eBuildErr = fmt.Errorf("build %s: %v\n%s", b, err, out)
				return
			}
		}
	})
	require.NoError(t, e2eBuildErr)
	return filepath.Join(e2eBinDir, "rportd"), filepath.Join(e2eBinDir, "rport")
}

func e2eFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// syncBuffer is a bytes.Buffer safe for a process writing to it while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// e2eServer is one running rportd process.
type e2eServer struct {
	t          *testing.T
	root       string
	configPath string
	clientPort int
	apiURL     string
	user, pass string
	cmd        *exec.Cmd
	done       chan struct{}
	log        *syncBuffer
	clients    []*exec.Cmd
}

const e2eKeySeed = "e2e-key-seed-0123456789abcdef"

// e2eConfig writes an rportd.conf in root with a client and an API listener on free ports; serverExtra and apiExtra
// go into [server] and [api] (auth settings), extra is appended ([storage], [vault]).
func e2eConfig(t *testing.T, root, dataDir, serverExtra, apiExtra, extra string) (configPath string, clientPort int, apiURL string) {
	t.Helper()
	clientPort = e2eFreePort(t)
	apiPort := e2eFreePort(t)
	tunnelPort := e2eFreePort(t)
	conf := fmt.Sprintf(`[server]
  address = "127.0.0.1:%d"
  url = "http://127.0.0.1:%d"
  key_seed = %q
  data_dir = %q
  used_ports = ['%d']
  excluded_ports = []
  allow_root = true
%s

[logging]
  log_level = "info"

[api]
  address = "127.0.0.1:%d"
  jwt_secret = "e2e-jwt-secret-0123456789abcdef0123"
  password_min_length = 8
%s

[pairing]
  enabled = false
%s
`, clientPort, clientPort, e2eKeySeed, dataDir, tunnelPort, serverExtra, apiPort, apiExtra, extra)
	configPath = filepath.Join(root, "rportd.conf")
	require.NoError(t, os.WriteFile(configPath, []byte(conf), 0o600))
	return configPath, clientPort, fmt.Sprintf("http://127.0.0.1:%d", apiPort)
}

// runRportd runs an rportd subcommand to completion and returns its output.
func runRportd(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	rportd, _ := e2eBinaries(t)
	cmd := exec.Command(rportd, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "rportd %s:\n%s", strings.Join(args, " "), out)
	return string(out)
}

func startE2EServer(t *testing.T, root, configPath string, clientPort int, apiURL, user, pass string) *e2eServer {
	t.Helper()
	rportd, _ := e2eBinaries(t)
	s := &e2eServer{t: t, root: root, configPath: configPath, clientPort: clientPort, apiURL: apiURL, user: user,
		pass: pass, log: &syncBuffer{}}
	s.cmd = exec.Command(rportd, "-c", configPath)
	s.cmd.Dir = root
	s.cmd.Stdout = s.log
	s.cmd.Stderr = s.log
	require.NoError(t, s.cmd.Start())
	s.done = make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(s.done) }()
	t.Cleanup(s.stop)
	s.waitReady()
	return s
}

func (s *e2eServer) waitReady() {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			s.t.Fatalf("rportd exited:\n%s", s.log.String())
		default:
		}
		if code, _ := s.rawAPI(http.MethodGet, "/status", nil); code == http.StatusOK {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.t.Fatalf("rportd did not become ready:\n%s", s.log.String())
}

func (s *e2eServer) stop() {
	for _, c := range s.clients {
		if c.ProcessState == nil {
			_ = c.Process.Signal(syscall.SIGTERM)
			_ = c.Wait()
		}
	}
	s.clients = nil
	select {
	case <-s.done:
		return
	default:
	}
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(20 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.done
		s.t.Errorf("rportd did not stop on SIGTERM:\n%s", s.log.String())
	}
}

// rawAPI calls the API with basic auth and returns the status and body.
func (s *e2eServer) rawAPI(method, path string, body any) (int, string) {
	s.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(s.t, err)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.apiURL+"/api/v1"+path, r)
	require.NoError(s.t, err)
	req.SetBasicAuth(s.user, s.pass)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// mustAPI calls the API, fails the test on a non-2xx status and decodes {"data": …} into out.
func (s *e2eServer) mustAPI(method, path string, body, out any) {
	s.t.Helper()
	code, raw := s.rawAPI(method, path, body)
	require.True(s.t, code >= 200 && code < 300, "%s %s: HTTP %d: %s", method, path, code, raw)
	if out != nil {
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		require.NoError(s.t, json.Unmarshal([]byte(raw), &env), raw)
		require.NoError(s.t, json.Unmarshal(env.Data, out), raw)
	}
}

// connectClient starts an rport client and waits until the server lists it as connected.
func (s *e2eServer) connectClient(id, authID, authPass string) {
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
	cmd := exec.Command(rport, "-c", cp)
	cmd.Dir = dir
	logBuf := &syncBuffer{}
	cmd.Stdout = logBuf
	cmd.Stderr = logBuf
	require.NoError(s.t, cmd.Start())
	s.clients = append(s.clients, cmd)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		code, body := s.rawAPI(http.MethodGet, "/clients/"+id, nil)
		if code == http.StatusOK && strings.Contains(body, `"connection_state":"connected"`) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.t.Fatalf("client %s did not connect:\n%s\nserver:\n%s", id, logBuf.String(), s.log.String())
}

// waitJob polls a client's command until it finishes and returns it.
func (s *e2eServer) waitJob(clientID, jid string) map[string]any {
	s.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var job map[string]any
		s.mustAPI(http.MethodGet, "/clients/"+clientID+"/commands/"+jid, nil, &job)
		if st, _ := job["status"].(string); st != "running" && st != "" {
			return job
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.t.Fatalf("command %s on %s did not finish:\n%s", jid, clientID, s.log.String())
	return nil
}

const (
	e2eAdmin     = "admin"
	e2eAdminPass = "admin-password-e2e-0"
)

// TestPostgresNodeEndToEnd runs one rportd node on Postgres the way an admin would: migrate as the schema owner
// granting the application role, add a user with the CLI, start the server as the application role, add a client
// credential, connect a real client, run a command, create a schedule and see it run, use the vault with the local
// key provider, and restart the server to see that everything is still there.
func TestPostgresNodeEndToEnd(t *testing.T) {
	d := pgtest.NewEmpty(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	keyFile := filepath.Join(root, "vault.key")
	require.NoError(t, os.WriteFile(keyFile, []byte(hex.EncodeToString(key)+"\n"), 0o600))
	dsnFile := filepath.Join(root, "dsn")
	require.NoError(t, os.WriteFile(dsnFile, []byte(d.AppURL+"\n"), 0o600))

	configPath, clientPort, apiURL := e2eConfig(t, root, dataDir, "", "", fmt.Sprintf(`
[storage]
  backend = "postgres"
  dsn_file = %q
  role = "all"

[vault]
  key_provider = "local"
  local_key_file = %q
`, dsnFile, keyFile))

	out := runRportd(t, "", "migrate", "-c", configPath, "--dsn", d.OwnerURL, "--grant-to", pgtest.AppRole)
	assert.Contains(t, out, "applied")
	t.Logf("rportd migrate: %s", strings.TrimSpace(out))
	out = runRportd(t, "", "migrate", "-c", configPath, "--dsn", d.OwnerURL)
	assert.Contains(t, out, "nothing to migrate")
	t.Logf("rportd migrate again: %s", strings.TrimSpace(out))
	// "rportd user add" reads the password from a terminal (TestUserAddPostgres covers it); add the admin the way it
	// does, through the user service on the application role
	app, err := postgres.Open(context.Background(), d.AppURL)
	require.NoError(t, err)
	defer app.Close()
	require.NoError(t, pgserver.AddUser(context.Background(), app, tenant.Default, e2eAdmin, e2eAdminPass, []string{"Administrators"}))

	s := startE2EServer(t, root, configPath, clientPort, apiURL, e2eAdmin, e2eAdminPass)
	files, err := filepath.Glob(filepath.Join(dataDir, "*.db"))
	require.NoError(t, err)
	assert.Empty(t, files, "a Postgres node creates no SQLite files")

	// a client credential through the API, then a real client connecting with it
	s.mustAPI(http.MethodPost, "/clients-auth", map[string]string{"id": "e2e-auth", "password": "e2e-client-password-0"}, nil)
	s.connectClient("e2e-client", "e2e-auth", "e2e-client-password-0")
	var clientList []map[string]any
	s.mustAPI(http.MethodGet, "/clients", nil, &clientList)
	require.Len(t, clientList, 1)
	assert.Equal(t, "e2e-client", clientList[0]["id"])
	t.Logf("GET /clients: id=%v connection_state=%v", clientList[0]["id"], clientList[0]["connection_state"])

	// a command on the client
	var started struct {
		JID string `json:"jid"`
	}
	s.mustAPI(http.MethodPost, "/clients/e2e-client/commands", map[string]any{"command": "echo hello-from-postgres", "timeout_sec": 10}, &started)
	job := s.waitJob("e2e-client", started.JID)
	require.Equal(t, "successful", job["status"], "%v", job)
	assert.Contains(t, fmt.Sprint(job["result"]), "hello-from-postgres")
	t.Logf("command %s: status=%v result=%v", started.JID, job["status"], job["result"])

	// a schedule, which the node runs (the background work runs in the default tenant)
	var sched map[string]any
	s.mustAPI(http.MethodPost, "/schedules", map[string]any{
		"name": "e2e every minute", "schedule": "* * * * *", "type": "command",
		"client_ids": []string{"e2e-client"}, "command": "echo scheduled-on-postgres", "timeout_sec": 10,
	}, &sched)
	schedID := fmt.Sprint(sched["id"])
	require.NotEmpty(t, schedID)
	require.Eventually(t, func() bool {
		var got map[string]any
		s.mustAPI(http.MethodGet, "/schedules/"+schedID, nil, &got)
		last, _ := got["last_execution"].(map[string]any)
		if last != nil && last["status"] == "successful" {
			t.Logf("schedule %s ran: %v", schedID, last)
			return true
		}
		return false
	}, 90*time.Second, time.Second, "the schedule did not run")

	// the vault: managed by the local key file, no passphrase
	var status map[string]any
	s.mustAPI(http.MethodGet, "/vault-admin", nil, &status)
	assert.Equal(t, "unlocked", status["status"], "%v", status)
	var stored struct {
		ID int `json:"id"`
	}
	s.mustAPI(http.MethodPost, "/vault", map[string]any{"client_id": "e2e-client", "key": "db-password", "value": "s3cret-in-vault", "type": "secret"}, &stored)
	var value map[string]any
	s.mustAPI(http.MethodGet, fmt.Sprintf("/vault/%d", stored.ID), nil, &value)
	assert.Equal(t, "s3cret-in-vault", value["value"])
	var raw string
	require.NoError(t, d.Owner.Get(&raw, "SELECT value FROM vault_values WHERE key = 'db-password'"))
	assert.NotContains(t, raw, "s3cret-in-vault", "stored encrypted")
	t.Logf("vault: status=%v, value %d read back, stored as %d bytes of ciphertext", status["status"], stored.ID, len(raw))

	// after a restart: the client reconnects, history, schedule and vault are still there
	s.stop()
	s = startE2EServer(t, root, configPath, clientPort, apiURL, e2eAdmin, e2eAdminPass)
	s.connectClient("e2e-client", "e2e-auth", "e2e-client-password-0")
	var jobs []map[string]any
	s.mustAPI(http.MethodGet, "/clients/e2e-client/commands", nil, &jobs)
	assert.NotEmpty(t, jobs)
	s.mustAPI(http.MethodGet, "/schedules/"+schedID, nil, nil)
	s.mustAPI(http.MethodGet, fmt.Sprintf("/vault/%d", stored.ID), nil, &value)
	assert.Equal(t, "s3cret-in-vault", value["value"])

	// the server refuses to start as the schema owner (row-level security would not apply)
	ownerConf := strings.Replace(mustRead(t, configPath), fmt.Sprintf("dsn_file = %q", dsnFile), fmt.Sprintf("dsn = %q", d.OwnerURL), 1)
	s.stop()
	require.NoError(t, os.WriteFile(configPath, []byte(ownerConf), 0o600))
	rportd, _ := e2eBinaries(t)
	refused, err := exec.Command(rportd, "-c", configPath).CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(refused), "row-level security")
	t.Logf("as the schema owner: %s", strings.TrimSpace(string(refused)))
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}
