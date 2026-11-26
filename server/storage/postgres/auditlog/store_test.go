package auditlog_test

import (
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	oss "github.com/pohcee/procy-server/server/auditlog"
	"github.com/pohcee/procy-server/server/repotest"
	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/tenant"

	"github.com/pohcee/procy-server/server/storage/postgres"
	"github.com/pohcee/procy-server/server/storage/postgres/auditlog"
	"github.com/pohcee/procy-server/server/storage/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestContract(t *testing.T) {
	repotest.AuditLog(t, func(t *testing.T) oss.Provider {
		d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
		return auditlog.New(d.App)
	})
}

func seed(t *testing.T, d *pgtest.DB) {
	t.Helper()
	s := auditlog.New(d.App)
	for _, id := range []tenant.ID{repotest.TenantA, repotest.TenantB} {
		require.NoError(t, s.Save(repotest.Ctx(id), &oss.Entry{Timestamp: time.Now(), Application: "client", Action: "create", Username: string(id)}))
	}
}

func TestRLSBackstop(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	seed(t, d)
	var tenants []string
	require.NoError(t, postgres.InTenant(repotest.Ctx(repotest.TenantA), d.App, func(tx *sqlx.Tx) error {
		return tx.Select(&tenants, "SELECT tenant_id FROM auditlog")
	}))
	assert.Equal(t, []string{"a"}, tenants)
	var n int
	require.NoError(t, d.App.Get(&n, "SELECT count(*) FROM auditlog"))
	assert.Zero(t, n)
}

func TestTenantDeleteCascades(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	seed(t, d)
	_, err := d.Owner.Exec("DELETE FROM tenants WHERE id = 'a'")
	require.NoError(t, err)
	var tenants []string
	require.NoError(t, d.Owner.Select(&tenants, "SELECT tenant_id FROM auditlog"))
	assert.Equal(t, []string{"b"}, tenants)
}

// Rows copied from SQLite may have NULL text columns.
func TestNullColumnsReadAsEmpty(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	_, err := d.Owner.Exec("INSERT INTO auditlog (tenant_id, timestamp, application, action) VALUES ('a', now(), 'client', 'create')")
	require.NoError(t, err)
	entries, err := auditlog.New(d.App).List(repotest.Ctx(repotest.TenantA), &query.ListOptions{Sorts: []query.SortOption{{Column: "username", IsASC: true}}})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "", entries[0].Username)
	assert.Equal(t, "", entries[0].Response)
}

// Equal sort values keep the order the entries were saved in.
func TestInsertionOrder(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	s := auditlog.New(d.App)
	ctx := repotest.Ctx(repotest.TenantA)
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, u := range []string{"c", "a", "b"} {
		require.NoError(t, s.Save(ctx, &oss.Entry{Timestamp: ts, Application: "client", Action: "create", Username: u}))
	}
	entries, err := s.List(ctx, &query.ListOptions{})
	require.NoError(t, err)
	require.Len(t, entries, 3)
	assert.Equal(t, []string{"c", "a", "b"}, []string{entries[0].Username, entries[1].Username, entries[2].Username})
	assert.Equal(t, time.UTC, entries[0].Timestamp.Location())
}
