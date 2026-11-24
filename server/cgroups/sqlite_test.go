package cgroups

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/migration/client_groups"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
)

func newTestProvider(t *testing.T) *SqliteProvider {
	t.Helper()
	db, err := sqlite.New(":memory:", client_groups.FS, sqlite.DataSourceOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	p, err := NewSqliteProvider(db)
	require.NoError(t, err)
	return p
}

func group(id string) *ClientGroup { return groupWithDesc(id, "") }

func groupWithDesc(id, desc string) *ClientGroup {
	return &ClientGroup{ID: id, Description: desc, Params: &ClientParams{}, AllowedUserGroups: []string{}}
}

func ctxFor(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

// migrateTo opens a file DB and migrates it to the given schema version (up or down).
func migrateTo(t *testing.T, path string, version uint) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("sqlite3", path)
	require.NoError(t, err)
	require.NoError(t, sqlite.MigrateTo(db, client_groups.FS, version))
	return db
}

func TestProviderRequiresTenant(t *testing.T) {
	p := newTestProvider(t)
	ctx := context.Background()
	_, err := p.Get(ctx, "g1")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.GetAll(ctx)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.List(ctx, &query.ListOptions{})
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.ErrorIs(t, p.Create(ctx, group("g1")), tenant.ErrMissing)
	assert.ErrorIs(t, p.Update(ctx, group("g1")), tenant.ErrMissing)
	assert.ErrorIs(t, p.Delete(ctx, "g1"), tenant.ErrMissing)
}

func TestProviderIsolatesTenants(t *testing.T) {
	p := newTestProvider(t)
	a, b := ctxFor("a"), ctxFor("b")
	require.NoError(t, p.Create(a, groupWithDesc("g1", "from a")))
	require.NoError(t, p.Create(b, groupWithDesc("g1", "from b")))

	got, err := p.Get(a, "g1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "from a", got.Description)

	all, err := p.GetAll(a)
	require.NoError(t, err)
	assert.Len(t, all, 1)

	listed, err := p.List(b, &query.ListOptions{Filters: []query.FilterOption{{
		Column: []string{"id"}, Operator: query.FilterOperatorTypeEQ, Values: []string{"g1"},
	}}})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, "from b", listed[0].Description)

	updated := groupWithDesc("g1", "a updated")
	require.NoError(t, p.Update(a, updated))
	still, err := p.Get(b, "g1")
	require.NoError(t, err)
	assert.Equal(t, "from b", still.Description)

	require.NoError(t, p.Delete(a, "g1"))
	gone, err := p.Get(a, "g1")
	require.NoError(t, err)
	assert.Nil(t, gone)
	still, err = p.Get(b, "g1")
	require.NoError(t, err)
	assert.NotNil(t, still)
}

func TestCreateIgnoresTenantOnStruct(t *testing.T) {
	p := newTestProvider(t)
	g := group("g1")
	g.TenantID = "b"
	require.NoError(t, p.Create(ctxFor("a"), g))
	got, err := p.Get(ctxFor("b"), "g1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestTenantIDNotInJSON(t *testing.T) {
	g := group("g1")
	g.TenantID = "a"
	b, err := json.Marshal(g)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "tenant")
}

func TestUpgradeKeepsGroupsInDefaultTenant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_groups.db")
	old := migrateTo(t, path, 2)
	_, err := old.Exec(`INSERT INTO client_groups (id, description, params, allowed_user_groups) VALUES ('legacy', 'old', '{}', '[]')`)
	require.NoError(t, err)
	require.NoError(t, old.Close())

	db, err := sqlite.New(path, client_groups.FS, sqlite.DataSourceOptions{})
	require.NoError(t, err)
	defer db.Close()
	p, err := NewSqliteProvider(db)
	require.NoError(t, err)

	got, err := p.Get(ctxFor(tenant.Default), "legacy")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "old", got.Description)
}

func TestDowngradeKeepsOnlyDefaultTenant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_groups.db")
	db, err := sqlite.New(path, client_groups.FS, sqlite.DataSourceOptions{})
	require.NoError(t, err)
	p, err := NewSqliteProvider(db)
	require.NoError(t, err)
	require.NoError(t, p.Create(ctxFor(tenant.Default), groupWithDesc("g1", "default")))
	require.NoError(t, p.Create(ctxFor("a"), groupWithDesc("g1", "a")))
	require.NoError(t, db.Close())

	down := migrateTo(t, path, 2)
	defer down.Close()
	var rows []struct {
		ID          string `db:"id"`
		Description string `db:"description"`
	}
	require.NoError(t, down.Select(&rows, `SELECT id, description FROM client_groups`))
	require.Len(t, rows, 1)
	assert.Equal(t, "g1", rows[0].ID)
	assert.Equal(t, "default", rows[0].Description)

	var n int
	require.NoError(t, down.Get(&n, `SELECT COUNT(*) FROM pragma_table_info('client_groups') WHERE name = 'tenant_id'`))
	assert.Zero(t, n)
}
