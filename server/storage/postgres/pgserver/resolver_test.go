package pgserver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api/session"
	"github.com/openrport/openrport/server/bearer"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	"github.com/openrport/openrport/share/tenant"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

const testSecret = "0123456789abcdef0123456789abcdef"

type fakeSessions struct{}

func (fakeSessions) Save(context.Context, session.APISession) (int64, error) { return 1, nil }

func token(t *testing.T, id tenant.ID, secret string) string {
	t.Helper()
	tok, err := bearer.CreateAuthToken(tenant.WithID(context.Background(), id), fakeSessions{}, secret, time.Hour, "alice", nil, "", "")
	require.NoError(t, err)
	return tok
}

func newResolver(t *testing.T) *Resolver {
	t.Helper()
	d := pgtest.New(t, "a", "s")
	_, err := d.Owner.Exec(`UPDATE tenants SET status = 'suspended' WHERE id = 's';
		INSERT INTO user_logins (username, tenant_id) VALUES ('alice', 'a'), ('bob', 's')`)
	require.NoError(t, err)
	return NewResolver(d.App, testSecret)
}

func resolve(t *testing.T, r *Resolver, req *http.Request) tenant.ID {
	t.Helper()
	id, err := r.Resolve(req)
	require.NoError(t, err)
	return id
}

func TestResolverBearerTid(t *testing.T) {
	r := newResolver(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/clients", nil)
	req.Header.Set("Authorization", "Bearer "+token(t, "a", testSecret))
	assert.Equal(t, tenant.ID("a"), resolve(t, r, req))

	ws := httptest.NewRequest(http.MethodGet, "/api/v1/ws/commands?access_token="+token(t, "a", testSecret), nil)
	assert.Equal(t, tenant.ID("a"), resolve(t, r, ws))
}

func TestResolverRejectsForeignOrBadTokens(t *testing.T) {
	r := newResolver(t)
	for name, tok := range map[string]string{
		"wrong secret":     token(t, "a", "another-secret-another-secret-xx"),
		"garbage":          "not-a-jwt",
		"unknown tenant":   token(t, "zzz", testSecret),
		"suspended tenant": token(t, "s", testSecret),
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/clients", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		assert.Equal(t, Unresolved, resolve(t, r, req), name)
	}
}

func TestResolverBasicAuthUsername(t *testing.T) {
	r := newResolver(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/clients", nil)
	req.SetBasicAuth("alice", "whatever")
	assert.Equal(t, tenant.ID("a"), resolve(t, r, req))

	req.SetBasicAuth("nobody", "x")
	assert.Equal(t, Unresolved, resolve(t, r, req))

	req.SetBasicAuth("bob", "x") // suspended tenant
	assert.Equal(t, Unresolved, resolve(t, r, req))
}

func TestResolverLoginBodyJSONAndForm(t *testing.T) {
	r := newResolver(t)
	jsonBody := `{"username":"alice","password":"p"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	assert.Equal(t, tenant.ID("a"), resolve(t, r, req))
	b, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.Equal(t, jsonBody, string(b), "the handler still reads the whole body")

	form := url.Values{"username": {"alice"}, "password": {"p"}}.Encode()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	assert.Equal(t, tenant.ID("a"), resolve(t, r, req))
	require.NoError(t, req.ParseForm())
	assert.Equal(t, "p", req.PostForm.Get("password"))

	req = httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(`{"username":`))
	req.Header.Set("Content-Type", "application/json")
	assert.Equal(t, Unresolved, resolve(t, r, req))
}

func TestResolverUnknownIsUnresolved(t *testing.T) {
	r := newResolver(t)
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/", nil),
		httptest.NewRequest(http.MethodGet, "/api/v1/status", nil),
		httptest.NewRequest(http.MethodPost, "/api/v1/clients", strings.NewReader(`{"username":"alice"}`)),
	} {
		req.Header.Set("Content-Type", "application/json")
		assert.Equal(t, Unresolved, resolve(t, r, req), req.URL.Path)
	}
}

func TestResolverDatabaseErrorIsError(t *testing.T) {
	r := newResolver(t)
	require.NoError(t, r.db.Close())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/clients", nil)
	req.SetBasicAuth("alice", "x")
	_, err := r.Resolve(req)
	assert.Error(t, err)
}
