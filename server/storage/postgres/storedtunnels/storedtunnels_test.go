package storedtunnels

import (
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ossstoredtunnels "github.com/pohcee/procy-server/server/clients/storedtunnels"
	"github.com/pohcee/procy-server/server/repotest"
	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/tenant"

	"github.com/pohcee/procy-server/server/storage/postgres"
	pgclients "github.com/pohcee/procy-server/server/storage/postgres/clients"
	"github.com/pohcee/procy-server/server/storage/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestStoredTunnelsContract(t *testing.T) {
	repotest.StoredTunnels(t, func(t *testing.T) repotest.StoredTunnelRepos {
		d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
		return repotest.StoredTunnelRepos{Clients: pgclients.New(d.App, nil), Tunnels: New(d.App)}
	})
}

type fixture struct {
	d       *pgtest.DB
	clients *pgclients.Store
	tunnels *Store
}

// setup returns stores in which client c1 has tunnel t1 in both tenants.
func setup(t *testing.T) fixture {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	f := fixture{d: d, clients: pgclients.New(d.App, nil), tunnels: New(d.App)}
	for _, id := range []string{"a", "b"} {
		ctx := repotest.Ctx(tenantID(id))
		require.NoError(t, f.clients.Save(ctx, repotest.StoreClient("c1", nil)))
		require.NoError(t, f.tunnels.Insert(ctx, &ossstoredtunnels.StoredTunnel{ID: "t1", ClientID: "c1", Name: id, CreatedAt: time.Now()}))
	}
	return f
}

func TestRLSHidesOtherTenantsTunnels(t *testing.T) {
	f := setup(t)
	var names []string
	require.NoError(t, postgres.InTenant(repotest.Ctx(repotest.TenantA), f.d.App, func(tx *sqlx.Tx) error {
		return tx.Select(&names, "SELECT name FROM stored_tunnels")
	}))
	assert.Equal(t, []string{"a"}, names)

	var n int
	require.NoError(t, f.d.App.Get(&n, "SELECT count(*) FROM stored_tunnels"))
	assert.Zero(t, n)
}

func TestDeletingTenantDeletesTunnels(t *testing.T) {
	f := setup(t)
	_, err := f.d.Owner.Exec("DELETE FROM tenants WHERE id = 'a'")
	require.NoError(t, err)
	var names []string
	require.NoError(t, f.d.Owner.Select(&names, "SELECT name FROM stored_tunnels"))
	assert.Equal(t, []string{"b"}, names)
}

func TestDeletingClientDeletesItsTunnels(t *testing.T) {
	f := setup(t)
	a := repotest.Ctx(repotest.TenantA)
	require.NoError(t, f.clients.Delete(a, "c1", nil))
	got, err := f.tunnels.List(a, "c1", &query.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, got)
	got, err = f.tunnels.List(repotest.Ctx(repotest.TenantB), "c1", &query.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

func TestSavingClientKeepsItsTunnels(t *testing.T) {
	f := setup(t)
	a := repotest.Ctx(repotest.TenantA)
	c := repotest.StoreClient("c1", nil)
	c.Name = "renamed"
	require.NoError(t, f.clients.Save(a, c))
	got, err := f.tunnels.List(a, "c1", &query.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

func TestInsertForUnknownClientFails(t *testing.T) {
	f := setup(t)
	err := f.tunnels.Insert(repotest.Ctx(repotest.TenantA), &ossstoredtunnels.StoredTunnel{ID: "t2", ClientID: "unknown", CreatedAt: time.Now()})
	assert.ErrorContains(t, err, "foreign key")
}

func TestInsertDuplicateIDFails(t *testing.T) {
	f := setup(t)
	err := f.tunnels.Insert(repotest.Ctx(repotest.TenantA), &ossstoredtunnels.StoredTunnel{ID: "t1", ClientID: "c1", CreatedAt: time.Now()})
	assert.ErrorContains(t, err, "duplicate key")
}

func tenantID(s string) tenant.ID { return tenant.ID(s) }
