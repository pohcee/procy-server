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

	// a value that looks like a hash is hashed too (a crafted hash would make every login with the id expensive)
	added, err = s.Add(ctx, &clientsauth.ClientAuth{ID: "c2", Password: stored})
	require.NoError(t, err)
	require.True(t, added)
	ok, err := s.Verify(ctx, "c2", []byte("secret"))
	require.NoError(t, err)
	assert.False(t, ok)
	ok, err = s.Verify(ctx, "c2", []byte(stored))
	require.NoError(t, err)
	assert.True(t, ok, "the value is the password")

	// a stored value with the hash prefix that isn't a valid hash is never compared as plaintext
	assert.False(t, CheckStored("$argon2id$garbage", []byte("$argon2id$garbage")))

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

// TestAuthIndexReadOnlyAfterPlainUpgrade: the application role got writes on the table from default privileges (as
// when migration 1000 runs on a database granted earlier, with a plain "rportd migrate"); the migration takes them
// back.
func TestAuthIndexReadOnlyAfterPlainUpgrade(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	_, err := d.Owner.Exec("GRANT INSERT, UPDATE, DELETE, TRUNCATE ON client_auth_ids TO " + pgtest.AppRole + "; DELETE FROM schema_migrations WHERE version = 1000")
	require.NoError(t, err)
	_, err = d.App.Exec("DELETE FROM client_auth_ids")
	require.NoError(t, err, "writable before the migration")
	_, err = migrate.Up(context.Background(), d.Owner)
	require.NoError(t, err)
	_, err = d.App.Exec("DELETE FROM client_auth_ids")
	assert.ErrorContains(t, err, "permission denied")
	var n int
	require.NoError(t, d.App.Get(&n, "SELECT count(*) FROM client_auth_ids"), "still readable")
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

// TestAuthIndexBackfill applies migration 1000 again on credentials stored without the index (as before the
// migration): the index gets them.
func TestAuthIndexBackfill(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	s := New(d.App)
	_, err := s.Add(repotest.Ctx(repotest.TenantA), &clientsauth.ClientAuth{ID: "ca", Password: "p"})
	require.NoError(t, err)
	_, err = s.Add(repotest.Ctx(repotest.TenantB), &clientsauth.ClientAuth{ID: "cb", Password: "p"})
	require.NoError(t, err)
	_, err = d.Owner.Exec("DELETE FROM client_auth_ids; DELETE FROM schema_migrations WHERE version = 1000")
	require.NoError(t, err)

	applied, err := migrate.Up(context.Background(), d.Owner)
	require.NoError(t, err)
	assert.Equal(t, []int64{1000}, applied)
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
