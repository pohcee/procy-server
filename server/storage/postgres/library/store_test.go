package library_test

import (
	"context"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	oss "github.com/pohcee/procy-server/server/library"
	"github.com/pohcee/procy-server/server/repotest"
	"github.com/pohcee/procy-server/share/tenant"

	"github.com/pohcee/procy-server/server/storage/postgres"
	"github.com/pohcee/procy-server/server/storage/postgres/library"
	"github.com/pohcee/procy-server/server/storage/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestContract(t *testing.T) {
	repotest.Library(t, func(t *testing.T) oss.Repository {
		d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
		return library.New(d.App)
	})
}

var t0 = time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

func entry(id, name string) (oss.Entry, oss.Version) {
	c := &oss.Content{Name: name, Tags: []string{}, Implementations: map[string]oss.Implementation{oss.OSLinux: {Kind: oss.KindCommand, Code: "uptime", TimeoutSec: 60}}}
	e := oss.Entry{ID: id, Name: name, Tags: []string{}, ActiveVersion: 1, OS: []string{oss.OSLinux}, CreatedAt: t0, CreatedBy: "u", UpdatedAt: t0, UpdatedBy: "u"}
	v := oss.Version{EntryID: id, Version: 1, CreatedAt: t0, CreatedBy: "u", Content: c}
	return e, v
}

var tables = []string{"library_entries", "library_entry_versions", "library_meta"}

func seed(t *testing.T, d *pgtest.DB) {
	t.Helper()
	s := library.New(d.App)
	for _, id := range []string{"a", "b"} {
		ctx := repotest.Ctx(tenant.ID(id))
		e, v := entry("id1", "A")
		require.NoError(t, s.Insert(ctx, e, v))
		require.NoError(t, postgres.InTenant(ctx, d.App, func(tx *sqlx.Tx) error {
			_, err := tx.Exec("INSERT INTO library_meta (tenant_id, key, value) VALUES ($1, 'k', $1)", id)
			return err
		}))
	}
}

func TestRLSBackstop(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	seed(t, d)
	for _, table := range tables {
		var tenants []string
		require.NoError(t, postgres.InTenant(repotest.Ctx(repotest.TenantA), d.App, func(tx *sqlx.Tx) error {
			return tx.Select(&tenants, "SELECT DISTINCT tenant_id FROM "+table)
		}))
		assert.Equal(t, []string{"a"}, tenants, table)
		var n int
		require.NoError(t, d.App.Get(&n, "SELECT count(*) FROM "+table))
		assert.Zero(t, n, table)
	}
}

func TestTenantDeleteCascades(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	seed(t, d)
	_, err := d.Owner.Exec("DELETE FROM tenants WHERE id = 'a'")
	require.NoError(t, err)
	for _, table := range tables {
		var tenants []string
		require.NoError(t, d.Owner.Select(&tenants, "SELECT DISTINCT tenant_id FROM "+table))
		assert.Equal(t, []string{"b"}, tenants, table)
	}
}

func TestMetaReadsImportedValue(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	seed(t, d)
	s := library.New(d.App)
	v, ok, err := s.Meta(repotest.Ctx(repotest.TenantB), "k")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "b", v)
}

func TestStoredJSONMatchesSQLite(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	s := library.New(d.App)
	e, v := entry("id1", "A")
	e.Tags = nil
	require.NoError(t, s.Insert(repotest.Ctx(repotest.TenantA), e, v))
	var row struct {
		Tags    string `db:"tags"`
		OS      string `db:"os"`
		Changed string `db:"changed"`
		Content string `db:"content"`
	}
	require.NoError(t, d.Owner.Get(&row, `SELECT e.tags, e.os, v.changed, v.content FROM library_entries e
		JOIN library_entry_versions v ON v.tenant_id = e.tenant_id AND v.entry_id = e.id`))
	assert.Equal(t, "[]", row.Tags)
	assert.Equal(t, `["linux"]`, row.OS)
	assert.Equal(t, "[]", row.Changed)
	assert.JSONEq(t, `{"name":"A","description":"","tags":[],"implementations":{"linux":{"kind":"command","code":"uptime","interpreter":"","is_sudo":false,"cwd":"","timeout_sec":60}}}`, row.Content)
}

func TestCloseLeavesDBOpen(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	require.NoError(t, library.New(d.App).Close())
	require.NoError(t, d.App.PingContext(context.Background()))
}
