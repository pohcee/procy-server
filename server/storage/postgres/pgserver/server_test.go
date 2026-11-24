package pgserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/clients/clientdata"
	pgclients "github.com/openrport/openrport/server/storage/postgres/clients"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
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

// startNode runs a Postgres server on a fresh database with tenants a (user alice, client web01 named "a-web") and b (user
// bob, client web01 named "b-web").
func startNode(t *testing.T) *nodeFixture { return startNodeWith(t, Options{}) }

func startNodeWith(t *testing.T, opts Options) *nodeFixture {
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
	cfg, _, err := LoadConfig(env(map[string]string{
		EnvDatabaseURL: d.AppURL, EnvJWTSecret: testSecret, EnvAPIAddress: addr, EnvDataDir: t.TempDir(),
		EnvLogLevel: "error",
	}))
	require.NoError(t, err)

	runCtx, cancel := context.WithCancel(ctx)
	s, err := NewServer(runCtx, cfg, d.App, opts)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- s.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("server did not stop")
		}
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
	h := startNode(t)
	tok := h.login(t, "alice")
	code, out := h.do(t, http.MethodPost, "/clients/web01/commands", tok, map[string]any{"command": "uptime"})
	assert.GreaterOrEqual(t, code, 400, out["_raw"])
	assert.Less(t, code, 500, out["_raw"])
	code, out = h.do(t, http.MethodGet, "/clients/web01/commands", tok, nil)
	assert.Equal(t, http.StatusOK, code, out["_raw"])
}

func TestNodeVaultNotAvailable(t *testing.T) {
	h := startNode(t)
	tok := h.login(t, "alice")
	code, out := h.do(t, http.MethodGet, "/vault-admin", tok, nil)
	assert.Equal(t, http.StatusOK, code, out["_raw"])
	code, out = h.do(t, http.MethodPost, "/vault-admin/init", tok, map[string]string{"password": "abcdefghijklmnop1234"})
	assert.Equal(t, http.StatusNotImplemented, code, out["_raw"])
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

// fixedKeys is a managed vault key provider with one key per tenant.
type fixedKeys struct{}

func (fixedKeys) Key(ctx context.Context) ([]byte, error) {
	id, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	return bytes.Repeat([]byte(id)[:1], 32), nil
}
func (fixedKeys) Unlocked(context.Context) bool { return true }

func TestNodeManagedVaultPerTenant(t *testing.T) {
	h := startNodeWith(t, Options{VaultKeys: fixedKeys{}})
	alice, bob := h.login(t, "alice"), h.login(t, "bob")
	code, out := h.do(t, http.MethodPost, "/vault", alice, map[string]any{"client_id": "web01", "key": "db", "value": "s3cret", "type": "secret"})
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	code, out = h.do(t, http.MethodGet, "/vault", alice, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Len(t, out["data"], 1)
	code, out = h.do(t, http.MethodGet, "/vault", bob, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Empty(t, out["data"], "bob's tenant sees none of alice's values")
}

func TestNodeRefusesPrivilegedRole(t *testing.T) {
	d := pgtest.New(t)
	cfg, _, err := LoadConfig(env(map[string]string{
		EnvDatabaseURL: d.AppURL, EnvJWTSecret: testSecret, EnvAPIAddress: freeAddr(t), EnvDataDir: t.TempDir(),
	}))
	require.NoError(t, err)
	_, err = NewServer(context.Background(), cfg, d.Owner, Options{})
	assert.ErrorContains(t, err, "row-level security")
}
