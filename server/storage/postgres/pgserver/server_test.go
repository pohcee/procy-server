package pgserver

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	chserver "github.com/openrport/openrport/server"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/clients/clientdata"
	pgclients "github.com/openrport/openrport/server/storage/postgres/clients"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	chshare "github.com/openrport/openrport/share"
	"github.com/openrport/openrport/share/files"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

const testPassword = "correct horse battery staple"

type nodeFixture struct {
	base string
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

// loadConfig decodes an rportd.conf the way rportd does: rportd's defaults, the TOML file, then validation.
func loadConfig(t *testing.T, conf string) *chconfig.Config {
	t.Helper()
	v := viper.New()
	v.SetConfigType("toml")
	chconfig.SetDefaults(v)
	cfg := &chconfig.Config{}
	require.NoError(t, chshare.DecodeViperConfig(v, cfg, strings.NewReader(conf)))
	mLog := logger.NewMemLogger()
	require.NoError(t, cfg.ParseAndValidate(&mLog))
	return cfg
}

type nodeSettings struct {
	role  string // [storage] role; default "api"
	vault string // extra [vault] settings
}

// nodeConf is the rportd.conf of a Postgres node with the API on apiAddr.
func nodeConf(t *testing.T, dsn, apiAddr string, s nodeSettings) string {
	t.Helper()
	if s.role == "" {
		s.role = chconfig.StorageRoleAPI
	}
	return fmt.Sprintf(`
[server]
  address = %q
  data_dir = %q
[logging]
  log_level = "error"
[api]
  address = %q
  jwt_secret = %q
[storage]
  backend = "postgres"
  dsn = %q
  role = %q
[vault]
%s
`, freeAddr(t), t.TempDir(), apiAddr, testSecret, dsn, s.role, s.vault)
}

func localKeyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vault.key")
	require.NoError(t, os.WriteFile(path, []byte(hex.EncodeToString(bytes.Repeat([]byte{9}, 32))+"\n"), 0600))
	return path
}

// startNode runs a Postgres node on a fresh database with tenants a (user alice, client web01 named "a-web") and b
// (user bob, client web01 named "b-web").
func startNode(t *testing.T) *nodeFixture { return startNodeWith(t, nodeSettings{}) }

func startNodeWith(t *testing.T, s nodeSettings) *nodeFixture {
	t.Helper()
	ctx := context.Background()
	d := pgtest.New(t)
	for _, tc := range []struct {
		id         tenant.ID
		user, name string
	}{{"a", "alice", "a-web"}, {"b", "bob", "b-web"}} {
		require.NoError(t, AddTenant(ctx, d.Owner, tc.id, ""))
		require.NoError(t, AddUser(ctx, d.App, tc.id, tc.user, testPassword, nil))
		now := time.Now()
		cl := &clientdata.Client{ID: "web01", ClientAuthID: "auth-" + string(tc.id), Name: tc.name, DisconnectedAt: &now}
		require.NoError(t, pgclients.New(d.App, nil).Save(tenant.WithID(ctx, tc.id), cl))
	}

	addr := freeAddr(t)
	cfg := loadConfig(t, nodeConf(t, d.AppURL, addr, s))

	runCtx, cancel := context.WithCancel(ctx)
	db, err := Open(runCtx, cfg)
	require.NoError(t, err)
	opts, closeOpts, err := NodeOpts(runCtx, cfg, db, &chserver.ServerOpts{FilesAPI: files.NewFileSystem()}, io.Discard)
	require.NoError(t, err)
	srv, err := chserver.NewServer(runCtx, cfg, opts)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- srv.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("server did not stop")
		}
		closeOpts()
		db.Close()
	})
	base := "http://" + addr + "/api/v1"
	// wait for the port with a plain dial: an unauthenticated request would delay the next one (user_login_wait)
	require.Eventually(t, func() bool {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}, 10*time.Second, 50*time.Millisecond)
	return &nodeFixture{base: base}
}

func (h *nodeFixture) do(t *testing.T, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.base+path, rd)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	out["_raw"] = string(raw)
	return resp.StatusCode, out
}

func (h *nodeFixture) login(t *testing.T, user string) string {
	t.Helper()
	code, out := h.do(t, http.MethodPost, "/login", "", map[string]string{"username": user, "password": testPassword})
	require.Equal(t, http.StatusOK, code, out["_raw"])
	tok, _ := out["data"].(map[string]any)["token"].(string)
	require.NotEmpty(t, tok)
	return tok
}

func clientNames(t *testing.T, out map[string]any) []string {
	t.Helper()
	var names []string
	list, ok := out["data"].([]any)
	require.True(t, ok, out["_raw"])
	for _, c := range list {
		names = append(names, fmt.Sprint(c.(map[string]any)["name"]))
	}
	return names
}

func TestNodeLoginAndListClientsPerTenant(t *testing.T) {
	h := startNode(t)
	for user, want := range map[string]string{"alice": "a-web", "bob": "b-web"} {
		tok := h.login(t, user)
		code, out := h.do(t, http.MethodGet, "/clients", tok, nil)
		require.Equal(t, http.StatusOK, code, out["_raw"])
		assert.Equal(t, []string{want}, clientNames(t, out), user)

		code, out = h.do(t, http.MethodGet, "/me", tok, nil)
		require.Equal(t, http.StatusOK, code, out["_raw"])
		assert.Equal(t, user, out["data"].(map[string]any)["username"])

		code, out = h.do(t, http.MethodGet, "/status", tok, nil)
		assert.Equal(t, http.StatusOK, code, out["_raw"])

		code, out = h.do(t, http.MethodGet, "/library/entries", tok, nil)
		assert.Equal(t, http.StatusOK, code, out["_raw"])
		assert.NotEmpty(t, out["data"], "built-in library entries synced for the tenant")
	}
}

func TestNodeLoginUnknownUser401(t *testing.T) {
	h := startNode(t)
	// each case on its own server: a failed attempt delays the next one (user_login_wait), as in standalone
	code, out := h.do(t, http.MethodPost, "/login", "", map[string]string{"username": "mallory", "password": testPassword})
	assert.Equal(t, http.StatusUnauthorized, code, out["_raw"], "unknown user: 401 as in standalone, not 404")
	code, out = startNode(t).do(t, http.MethodGet, "/clients", "", nil)
	assert.Equal(t, http.StatusUnauthorized, code, out["_raw"])
	code, out = startNode(t).do(t, http.MethodPost, "/login", "", map[string]string{"username": "alice", "password": "wrong password here"})
	assert.Equal(t, http.StatusUnauthorized, code, out["_raw"])
}

func TestNodeCommandNotConnected(t *testing.T) {
	h := startNode(t) // role "api"
	tok := h.login(t, "alice")
	code, out := h.do(t, http.MethodPost, "/clients/web01/commands", tok, map[string]any{"command": "uptime"})
	assert.GreaterOrEqual(t, code, 400, out["_raw"])
	assert.Less(t, code, 500, out["_raw"])
	code, out = h.do(t, http.MethodGet, "/clients/web01/commands", tok, nil)
	assert.Equal(t, http.StatusOK, code, out["_raw"])
}

func TestNodeRoleAllServesAPI(t *testing.T) {
	h := startNodeWith(t, nodeSettings{role: chconfig.StorageRoleAll})
	tok := h.login(t, "alice")
	code, out := h.do(t, http.MethodGet, "/clients", tok, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, []string{"a-web"}, clientNames(t, out))
}

func TestNodePassphraseVault(t *testing.T) {
	h := startNode(t) // [vault] key_provider defaults to "passphrase"
	alice, bob := h.login(t, "alice"), h.login(t, "bob")
	code, out := h.do(t, http.MethodPost, "/vault", alice, map[string]any{"client_id": "web01", "key": "db", "value": "s3cret", "type": "secret"})
	assert.GreaterOrEqual(t, code, 400, "not initialised yet: %s", out["_raw"])

	code, out = h.do(t, http.MethodPost, "/vault-admin/init", alice, map[string]string{"password": "abcdefghijklmnop1234"})
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	code, out = h.do(t, http.MethodPost, "/vault", alice, map[string]any{"client_id": "web01", "key": "db", "value": "s3cret", "type": "secret"})
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	code, out = h.do(t, http.MethodGet, "/vault", alice, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Len(t, out["data"], 1)

	// the vault of another tenant is its own: not initialised
	code, out = h.do(t, http.MethodGet, "/vault-admin", bob, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, "uninitialized", out["data"].(map[string]any)["init"], out["_raw"])
}

func TestNodeManagedVaultPerTenant(t *testing.T) {
	h := startNodeWith(t, nodeSettings{vault: fmt.Sprintf("key_provider = \"local\"\nlocal_key_file = %q\n", localKeyFile(t))})
	alice, bob := h.login(t, "alice"), h.login(t, "bob")
	code, out := h.do(t, http.MethodPost, "/vault", alice, map[string]any{"client_id": "web01", "key": "db", "value": "s3cret", "type": "secret"})
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	code, out = h.do(t, http.MethodGet, "/vault", alice, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Len(t, out["data"], 1)
	code, out = h.do(t, http.MethodGet, "/vault", bob, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Empty(t, out["data"], "bob's tenant sees none of alice's values")
	code, out = h.do(t, http.MethodPost, "/vault-admin/init", alice, map[string]string{"password": "abcdefghijklmnop1234"})
	assert.Equal(t, http.StatusConflict, code, "a managed vault needs no passphrase: %s", out["_raw"])
}

func TestNodeAPITokenBasicAuth(t *testing.T) {
	h := startNode(t)
	tok := h.login(t, "alice")
	code, out := h.do(t, http.MethodPost, "/me/tokens", tok, map[string]string{"scope": "read", "name": "ci"})
	require.Equal(t, http.StatusOK, code, out["_raw"])
	apiToken := out["data"].(map[string]any)["token"].(string)

	req, err := http.NewRequest(http.MethodGet, h.base+"/clients", nil)
	require.NoError(t, err)
	req.SetBasicAuth("alice", apiToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestPostgresRefusesOwnerRole(t *testing.T) {
	d := pgtest.New(t)
	cfg := loadConfig(t, nodeConf(t, d.OwnerURL, freeAddr(t), nodeSettings{}))
	db, err := Open(context.Background(), cfg)
	require.NoError(t, err)
	defer db.Close()
	_, _, err = NodeOpts(context.Background(), cfg, db, nil, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "row-level security")
	assert.Contains(t, err.Error(), "connect as the application role")
}

func TestPostgresRefusesUnmigrated(t *testing.T) {
	d := pgtest.NewEmpty(t)
	cfg := loadConfig(t, nodeConf(t, d.AppURL, freeAddr(t), nodeSettings{}))
	db, err := Open(context.Background(), cfg)
	require.NoError(t, err)
	defer db.Close()
	_, _, err = NodeOpts(context.Background(), cfg, db, nil, io.Discard)
	assert.ErrorContains(t, err, `run "rportd migrate"`)
}

func TestServerOptsRoleAPI(t *testing.T) {
	d := pgtest.New(t)
	fs := files.NewFileSystem()
	base := &chserver.ServerOpts{FilesAPI: fs}

	cfg := loadConfig(t, nodeConf(t, d.AppURL, freeAddr(t), nodeSettings{role: chconfig.StorageRoleAPI}))
	opts, closeOpts, err := NodeOpts(context.Background(), cfg, d.App, base, io.Discard)
	require.NoError(t, err)
	defer closeOpts()
	assert.True(t, opts.DisableClientListener)
	assert.Equal(t, NotConnectedDispatcher{}, opts.Dispatcher)
	assert.True(t, opts.SkipMigrations)
	assert.NotNil(t, opts.TenantResolver)
	assert.NotNil(t, opts.Repositories.Users)
	assert.Nil(t, opts.VaultKeys, "passphrase vault by default")
	assert.Same(t, fs, opts.FilesAPI, "copied from base")
	assert.False(t, base.SkipMigrations, "base is not modified")

	cfg = loadConfig(t, nodeConf(t, d.AppURL, freeAddr(t), nodeSettings{role: chconfig.StorageRoleAll}))
	opts, closeOpts, err = NodeOpts(context.Background(), cfg, d.App, base, io.Discard)
	require.NoError(t, err)
	defer closeOpts()
	assert.False(t, opts.DisableClientListener)
	assert.Nil(t, opts.Dispatcher, "role all does the work on its own connected clients")

	cfg = loadConfig(t, nodeConf(t, d.AppURL, freeAddr(t), nodeSettings{vault: fmt.Sprintf("key_provider = \"local\"\nlocal_key_file = %q\n", localKeyFile(t))}))
	opts, closeOpts, err = NodeOpts(context.Background(), cfg, d.App, base, io.Discard)
	require.NoError(t, err)
	defer closeOpts()
	assert.NotNil(t, opts.VaultKeys)
}
