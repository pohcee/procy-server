package clientauth

import (
	"context"
	"database/sql"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/clientsauth"
	"github.com/openrport/openrport/server/repotest"
	"github.com/openrport/openrport/share/enums"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"

	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/migrate"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestContract(t *testing.T) {
	repotest.ClientAuth(t, func(t *testing.T) clientsauth.Provider {
		return New(pgtest.New(t, repotest.TenantA, repotest.TenantB).App)
	}, repotest.ClientAuthOptions{})
}

func TestProviderKind(t *testing.T) {
	s := New(nil)
	assert.True(t, s.IsWriteable())
	assert.Equal(t, enums.ProviderSourceDB, s.Source())
}

func TestIDsUniqueAcrossTenants(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	s := New(d.App)
	a, b := repotest.Ctx(repotest.TenantA), repotest.Ctx(repotest.TenantB)
	added, err := s.Add(a, &clientsauth.ClientAuth{ID: "c1", Password: "pa"})
	require.NoError(t, err)
	require.True(t, added)

	added, err = s.Add(b, &clientsauth.ClientAuth{ID: "c1", Password: "pb"})
	require.NoError(t, err)
	assert.False(t, added, "an id of another tenant counts as existing")
	got, err := s.Get(b, "c1")
	require.NoError(t, err)
	assert.Nil(t, got)
	got, err = s.Get(a, "c1")
	require.NoError(t, err)
	assert.Equal(t, "pa", got.Password)
}

func TestGetFilteredRefusesUnknownFilter(t *testing.T) {
	s := New(pgtest.New(t, repotest.TenantA).App)
	_, _, err := s.GetFiltered(repotest.Ctx(repotest.TenantA), &query.ListOptions{
		Filters: []query.FilterOption{{Column: []string{"password; --"}, Operator: query.FilterOperatorTypeEQ, Values: []string{"x"}}},
	})
	assert.ErrorContains(t, err, "unsupported filter")
}

func TestGetFilteredDoesNotChangeOptions(t *testing.T) {
	s := New(pgtest.New(t, repotest.TenantA).App)
	opts := &query.ListOptions{Pagination: query.NewPagination(5, 0)}
	_, _, err := s.GetFiltered(repotest.Ctx(repotest.TenantA), opts)
	require.NoError(t, err)
	assert.NotNil(t, opts.Pagination)
	assert.Empty(t, opts.Sorts)

	// without pagination everything is returned
	_, err = s.Add(repotest.Ctx(repotest.TenantA), &clientsauth.ClientAuth{ID: "c1", Password: "p"})
	require.NoError(t, err)
	list, count, err := s.GetFiltered(repotest.Ctx(repotest.TenantA), &query.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.Len(t, list, 1)
}

func TestRLSBackstop(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	s := New(d.App)
	a, b := repotest.Ctx(repotest.TenantA), repotest.Ctx(repotest.TenantB)
	_, err := s.Add(a, &clientsauth.ClientAuth{ID: "ca", Password: "p"})
	require.NoError(t, err)
	_, err = s.Add(b, &clientsauth.ClientAuth{ID: "cb", Password: "p"})
	require.NoError(t, err)

	var ids []string
	require.NoError(t, postgres.InTenant(a, d.App, func(tx *sqlx.Tx) error {
		return tx.Select(&ids, "SELECT id FROM clients_auth")
	}))
	assert.Equal(t, []string{"ca"}, ids)
	var n int
	require.NoError(t, d.App.Get(&n, "SELECT count(*) FROM clients_auth"))
	assert.Zero(t, n)
}

func TestDeletingTenantCascades(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	s := New(d.App)
	_, err := s.Add(repotest.Ctx(repotest.TenantA), &clientsauth.ClientAuth{ID: "ca", Password: "p"})
	require.NoError(t, err)
	_, err = s.Add(repotest.Ctx(repotest.TenantB), &clientsauth.ClientAuth{ID: "cb", Password: "p"})
	require.NoError(t, err)
	_, err = d.Owner.Exec("DELETE FROM tenants WHERE id = 'a'")
	require.NoError(t, err)
	var ids []string
	require.NoError(t, d.Owner.Select(&ids, "SELECT id FROM clients_auth"))
	assert.Equal(t, []string{"cb"}, ids)
	// the id is free again
	added, err := s.Add(repotest.Ctx(repotest.TenantB), &clientsauth.ClientAuth{ID: "ca", Password: "p"})
	require.NoError(t, err)
	assert.True(t, added)
}

func TestTenantOfAuthID(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	s := New(d.App)
	a, b := repotest.Ctx(repotest.TenantA), repotest.Ctx(repotest.TenantB)
	_, err := s.Add(a, &clientsauth.ClientAuth{ID: "ca", Password: "p"})
	require.NoError(t, err)
	_, err = s.Add(b, &clientsauth.ClientAuth{ID: "cb", Password: "p"})
	require.NoError(t, err)

	// no tenant in ctx: the index has no row-level security
	ctx := context.Background()
	for id, want := range map[string]tenant.ID{"ca": repotest.TenantA, "cb": repotest.TenantB} {
		got, found, err := s.TenantOf(ctx, id)
		require.NoError(t, err)
		assert.True(t, found, id)
		assert.Equal(t, want, got, id)
	}
	_, found, err := s.TenantOf(ctx, "nope")
	require.NoError(t, err)
	assert.False(t, found)

	// a deleted credential leaves the index; its id is free for another tenant
	require.NoError(t, s.Delete(a, "ca"))
	_, found, err = s.TenantOf(ctx, "ca")
	require.NoError(t, err)
	assert.False(t, found)
	added, err := s.Add(b, &clientsauth.ClientAuth{ID: "ca", Password: "p"})
	require.NoError(t, err)
	require.True(t, added)
	got, _, err := s.TenantOf(ctx, "ca")
	require.NoError(t, err)
	assert.Equal(t, repotest.TenantB, got)

	// a deleted tenant takes its ids with it
	_, err = d.Owner.Exec("DELETE FROM tenants WHERE id = $1", string(repotest.TenantB))
	require.NoError(t, err)
	_, found, err = s.TenantOf(ctx, "cb")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestAuthIndexAppRoleReadOnly(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	for _, q := range []string{
		"INSERT INTO client_auth_ids (id, tenant_id) VALUES ('x', 'a')",
		"DELETE FROM client_auth_ids",
		"UPDATE client_auth_ids SET tenant_id = 'a'",
	} {
		_, err := d.App.Exec(q)
		assert.ErrorContains(t, err, "permission denied", q)
	}
}

// TestAuthIndexBackfill applies migration 0210 again on credentials stored without the index (as before the
// migration): the index gets them.
func TestAuthIndexBackfill(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	s := New(d.App)
	_, err := s.Add(repotest.Ctx(repotest.TenantA), &clientsauth.ClientAuth{ID: "ca", Password: "p"})
	require.NoError(t, err)
	_, err = s.Add(repotest.Ctx(repotest.TenantB), &clientsauth.ClientAuth{ID: "cb", Password: "p"})
	require.NoError(t, err)
	_, err = d.Owner.Exec("DELETE FROM client_auth_ids; DELETE FROM schema_migrations WHERE version = 210")
	require.NoError(t, err)

	applied, err := migrate.Up(context.Background(), d.Owner)
	require.NoError(t, err)
	assert.Equal(t, []int64{210}, applied)
	for id, want := range map[string]tenant.ID{"ca": repotest.TenantA, "cb": repotest.TenantB} {
		got, found, err := s.TenantOf(context.Background(), id)
		require.NoError(t, err)
		assert.True(t, found, id)
		assert.Equal(t, want, got, id)
	}
	// row-level security is forced again afterwards
	var forced bool
	require.NoError(t, d.Owner.Get(&forced, "SELECT relforcerowsecurity FROM pg_class WHERE relname = 'clients_auth'"))
	assert.True(t, forced)
}

func TestMaxClientsColumn(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	var limit sql.NullInt64
	require.NoError(t, d.App.Get(&limit, "SELECT max_clients FROM tenants WHERE id = 'a'"))
	assert.False(t, limit.Valid, "no limit by default")
	_, err := d.Owner.Exec("UPDATE tenants SET max_clients = -1 WHERE id = 'a'")
	assert.Error(t, err)
	_, err = d.Owner.Exec("UPDATE tenants SET max_clients = 0 WHERE id = 'a'")
	assert.NoError(t, err)
}
