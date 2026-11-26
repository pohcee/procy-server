package authorization

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/db/migration/api_token"
	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/share/tenant"
)

func ctxFor(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

func newTenantTestProvider(t *testing.T) *SqliteProvider {
	t.Helper()
	db, err := sqlite.New(":memory:", api_token.FS, DataSourceOptions)
	require.NoError(t, err)
	p := NewSqliteProvider(db)
	t.Cleanup(func() { p.Close() })
	return p
}

func tok(username, prefix, name, token string) *APIToken {
	return &APIToken{Username: username, Prefix: prefix, Name: name, Scope: APITokenRead, Token: token}
}

// migrateTokensTo opens a file DB and migrates it to the given schema version (up or down).
func migrateTokensTo(t *testing.T, path string, version uint) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("sqlite3", path)
	require.NoError(t, err)
	require.NoError(t, sqlite.MigrateTo(db, api_token.FS, version))
	return db
}

func TestTokenProviderRequiresTenant(t *testing.T) {
	p := newTenantTestProvider(t)
	ctx := context.Background()
	_, err := p.Get(ctx, "u", "prefix01")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.GetByName(ctx, "u", "n")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.GetAll(ctx, "u")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.ErrorIs(t, p.Save(ctx, tok("u", "prefix01", "n", "t")), tenant.ErrMissing)
	assert.ErrorIs(t, p.Delete(ctx, "u", "prefix01"), tenant.ErrMissing)
}

func TestTokenProviderIsolatesTenants(t *testing.T) {
	p := newTenantTestProvider(t)
	a, b := ctxFor("a"), ctxFor("b")
	// same username, prefix and name in both tenants
	require.NoError(t, p.Save(a, tok("u", "prefix01", "n", "token-a")))
	require.NoError(t, p.Save(b, tok("u", "prefix01", "n", "token-b")))

	got, err := p.Get(a, "u", "prefix01")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "token-a", got.Token)

	byName, err := p.GetByName(b, "u", "n")
	require.NoError(t, err)
	require.NotNil(t, byName)
	assert.Equal(t, "token-b", byName.Token)

	all, err := p.GetAll(a, "u")
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "token-a", all[0].Token)

	// update in a only
	require.NoError(t, p.Save(a, &APIToken{Username: "u", Prefix: "prefix01", Name: "renamed"}))
	still, err := p.Get(b, "u", "prefix01")
	require.NoError(t, err)
	assert.Equal(t, "n", still.Name)

	require.NoError(t, p.Delete(a, "u", "prefix01"))
	gone, err := p.Get(a, "u", "prefix01")
	require.NoError(t, err)
	assert.Nil(t, gone)
	still, err = p.Get(b, "u", "prefix01")
	require.NoError(t, err)
	assert.NotNil(t, still)

	// deleting in a tenant that doesn't have it fails like a missing token
	assert.Error(t, p.Delete(a, "u", "prefix01"))
}

func TestTokenSaveIgnoresTenantOnStruct(t *testing.T) {
	p := newTenantTestProvider(t)
	tk := tok("u", "prefix01", "n", "t")
	tk.TenantID = "b"
	require.NoError(t, p.Save(ctxFor("a"), tk))
	got, err := p.Get(ctxFor("b"), "u", "prefix01")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestTokenTenantIDNotInJSON(t *testing.T) {
	tk := tok("u", "prefix01", "n", "t")
	tk.TenantID = "a"
	b, err := json.Marshal(tk)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "tenant")
}

func TestTokenUpgradeKeepsTokensInDefaultTenant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_token.db")
	old := migrateTokensTo(t, path, 3)
	_, err := old.Exec(`INSERT INTO api_tokens (username, prefix, name, scope, token) VALUES ('legacy', 'prefix01', 'old', 'read', 'secret')`)
	require.NoError(t, err)
	require.NoError(t, old.Close())

	db, err := sqlite.New(path, api_token.FS, DataSourceOptions)
	require.NoError(t, err)
	p := NewSqliteProvider(db)
	defer p.Close()

	got, err := p.Get(ctxFor(tenant.Default), "legacy", "prefix01")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "old", got.Name)
	assert.Equal(t, "secret", got.Token)

	var n int
	require.NoError(t, db.Get(&n, `SELECT COUNT(*) FROM api_tokens WHERE tenant_id = 'default'`))
	assert.Equal(t, 1, n)
}

func TestTokenDowngradeKeepsOnlyDefaultTenant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_token.db")
	db, err := sqlite.New(path, api_token.FS, DataSourceOptions)
	require.NoError(t, err)
	p := NewSqliteProvider(db)
	require.NoError(t, p.Save(ctxFor(tenant.Default), tok("u", "prefix01", "n", "default")))
	require.NoError(t, p.Save(ctxFor("a"), tok("u", "prefix01", "n", "a")))
	var before int
	require.NoError(t, db.Get(&before, `SELECT COUNT(*) FROM api_tokens`))
	require.Equal(t, 2, before)
	require.NoError(t, db.Close())

	down := migrateTokensTo(t, path, 3)
	defer down.Close()
	var rows []struct {
		Username string `db:"username"`
		Token    string `db:"token"`
	}
	require.NoError(t, down.Select(&rows, `SELECT username, token FROM api_tokens`))
	require.Len(t, rows, 1)
	assert.Equal(t, "default", rows[0].Token)

	var n int
	require.NoError(t, down.Get(&n, `SELECT COUNT(*) FROM pragma_table_info('api_tokens') WHERE name = 'tenant_id'`))
	assert.Zero(t, n)
	// the unique name index is back in its old shape
	require.NoError(t, down.Get(&n, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'api_tokens_unique_name'`))
	assert.Equal(t, 1, n)
}
