package apitokens

import (
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api/authorization"
	"github.com/openrport/openrport/server/repotest"

	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestContract(t *testing.T) {
	repotest.APITokens(t, func(t *testing.T) authorization.DbProvider {
		return New(pgtest.New(t, repotest.TenantA, repotest.TenantB).App)
	})
}

func token(username, prefix, name string) *authorization.APIToken {
	return &authorization.APIToken{Username: username, Prefix: prefix, Name: name, Scope: authorization.APITokenRead, Token: "hash"}
}

func TestRLSBackstop(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	s := New(d.App)
	a, b := repotest.Ctx(repotest.TenantA), repotest.Ctx(repotest.TenantB)
	require.NoError(t, s.Save(a, token("u", "prefix01", "n")))
	require.NoError(t, s.Save(b, token("u", "prefix01", "n")))

	var tenants []string
	require.NoError(t, postgres.InTenant(a, d.App, func(tx *sqlx.Tx) error {
		return tx.Select(&tenants, "SELECT tenant_id FROM api_tokens")
	}))
	assert.Equal(t, []string{"a"}, tenants)
	var n int
	require.NoError(t, d.App.Get(&n, "SELECT count(*) FROM api_tokens"))
	assert.Zero(t, n)
}

func TestDeletingTenantCascades(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	s := New(d.App)
	require.NoError(t, s.Save(repotest.Ctx(repotest.TenantA), token("u", "prefix01", "n")))
	require.NoError(t, s.Save(repotest.Ctx(repotest.TenantB), token("u", "prefix01", "n")))
	_, err := d.Owner.Exec("DELETE FROM tenants WHERE id = 'a'")
	require.NoError(t, err)
	var tenants []string
	require.NoError(t, d.Owner.Select(&tenants, "SELECT tenant_id FROM api_tokens"))
	assert.Equal(t, []string{"b"}, tenants)
}

func TestNullScopeReadsEmpty(t *testing.T) {
	// rows copied from old SQLite databases may have no scope
	d := pgtest.New(t, repotest.TenantA)
	_, err := d.Owner.Exec("INSERT INTO api_tokens (tenant_id, username, prefix, name, token) VALUES ('a', 'u', 'prefix01', 'n', 'hash')")
	require.NoError(t, err)
	got, err := New(d.App).Get(repotest.Ctx(repotest.TenantA), "u", "prefix01")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, authorization.APITokenScope(""), got.Scope)
}
