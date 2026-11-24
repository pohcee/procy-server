package clientauth

import (
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/clientsauth"
	"github.com/openrport/openrport/server/repotest"
	"github.com/openrport/openrport/share/enums"
	"github.com/openrport/openrport/share/query"

	"github.com/openrport/openrport/server/storage/postgres"
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
