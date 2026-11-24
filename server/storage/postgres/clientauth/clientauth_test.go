package clientauth

import (
	"context"
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
	}, repotest.ClientAuthOptions{Hashed: true})
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
	ok, err := s.Verify(a, "c1", []byte("pa"))
	require.NoError(t, err)
	assert.True(t, ok, "the first tenant's password is kept")
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

func TestPasswordsStoredHashed(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	s := New(d.App)
	ctx := repotest.Ctx(repotest.TenantA)
	in := &clientsauth.ClientAuth{ID: "c1", Password: "secret"}
	added, err := s.Add(ctx, in)
	require.NoError(t, err)
	require.True(t, added)
	assert.Equal(t, "secret", in.Password, "the caller's value is not changed")

	var stored string
	require.NoError(t, d.Owner.Get(&stored, "SELECT password FROM clients_auth WHERE id = 'c1'"))
	assert.True(t, clientsauth.IsHash(stored), stored)
	assert.True(t, CheckStored(stored, []byte("secret")))
	assert.False(t, CheckStored(stored, []byte("Secret")))

	// a hash is stored as it is (copied from another cluster database)
	added, err = s.Add(ctx, &clientsauth.ClientAuth{ID: "c2", Password: stored})
	require.NoError(t, err)
	require.True(t, added)
	ok, err := s.Verify(ctx, "c2", []byte("secret"))
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = s.Verify(ctx, "missing", []byte("secret"))
	require.NoError(t, err)
	assert.False(t, ok)
	ok, err = s.Verify(repotest.Ctx(repotest.TenantB), "c1", []byte("secret"))
	require.NoError(t, err)
	assert.False(t, ok, "another tenant's credentials don't verify")
}

func TestHashStored(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	_, err := d.Owner.Exec(`INSERT INTO clients_auth (tenant_id, id, password) VALUES ('a', 'old-a', 'pa'), ('b', 'old-b', 'pb')`)
	require.NoError(t, err)
	s := New(d.App)
	ok, err := s.Verify(repotest.Ctx(repotest.TenantA), "old-a", []byte("pa"))
	require.NoError(t, err)
	assert.True(t, ok, "a password stored before hashing still verifies")

	n, err := HashStored(context.Background(), d.Owner)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	n, err = HashStored(context.Background(), d.Owner)
	require.NoError(t, err)
	assert.Zero(t, n, "nothing left to hash")

	ok, err = s.Verify(repotest.Ctx(repotest.TenantB), "old-b", []byte("pb"))
	require.NoError(t, err)
	assert.True(t, ok)
	var plain int
	require.NoError(t, d.Owner.Get(&plain, "SELECT count(*) FROM clients_auth WHERE password NOT LIKE '$argon2id$%'"))
	assert.Zero(t, plain)
}
