package users

import (
	"context"
	"net/http"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apierrors "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/repotest"
	"github.com/openrport/openrport/share/enums"
	"github.com/openrport/openrport/share/tenant"

	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func newStore(t *testing.T) (*Store, *pgtest.DB) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	return New(d.App), d
}

func TestContract(t *testing.T) {
	repotest.Users(t, func(t *testing.T) users.Provider {
		s, _ := newStore(t)
		return s
	}, repotest.UsersOptions{})
}

func TestProviderType(t *testing.T) {
	s := New(nil)
	assert.Equal(t, enums.ProviderSourceDB, s.Type())
	assert.True(t, s.SupportsGroupPermissions())
	assert.NoError(t, s.Close())
}

func TestUserLoginsUniqueAcrossTenants(t *testing.T) {
	s, _ := newStore(t)
	a, b := repotest.Ctx(repotest.TenantA), repotest.Ctx(repotest.TenantB)
	require.NoError(t, s.Add(a, &users.User{Username: "alice", Password: "p", Groups: []string{"g"}}))

	assertTaken := func(err error) {
		t.Helper()
		var apiErr apierrors.APIError
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.HTTPStatus)
		assert.Contains(t, apiErr.Message, `"alice" is already taken`)
	}
	assertTaken(s.Add(b, &users.User{Username: "alice", Password: "p"}))
	assertTaken(s.Add(a, &users.User{Username: "alice", Password: "p"}))

	// the failed add left nothing behind in b
	u, err := s.GetByUsername(b, "alice")
	require.NoError(t, err)
	assert.Nil(t, u)

	// renaming to a login of another tenant is refused too, and changes nothing
	require.NoError(t, s.Add(b, &users.User{Username: "bob", Password: "p"}))
	assertTaken(s.Update(b, &users.User{Username: "alice", Password: "new"}, "bob"))
	bob, err := s.GetByUsername(b, "bob")
	require.NoError(t, err)
	require.NotNil(t, bob)
	assert.Equal(t, "p", bob.Password)

	// once a deletes alice, b may take the login
	require.NoError(t, s.Delete(a, "alice"))
	require.NoError(t, s.Add(b, &users.User{Username: "alice", Password: "p"}))
}

func TestLookupTenantByUsername(t *testing.T) {
	s, _ := newStore(t)
	a, b := repotest.Ctx(repotest.TenantA), repotest.Ctx(repotest.TenantB)
	require.NoError(t, s.Add(a, &users.User{Username: "alice", Password: "p"}))
	require.NoError(t, s.Add(b, &users.User{Username: "bob", Password: "p"}))

	// no tenant in the context
	bg := context.Background()
	assertTenant := func(username string, want tenant.ID, wantFound bool) {
		t.Helper()
		got, found, err := s.TenantOf(bg, username)
		require.NoError(t, err)
		assert.Equal(t, wantFound, found, username)
		assert.Equal(t, want, got, username)
	}
	assertTenant("alice", repotest.TenantA, true)
	assertTenant("bob", repotest.TenantB, true)
	assertTenant("nobody", "", false)

	require.NoError(t, s.Update(a, &users.User{Username: "alice2"}, "alice"))
	assertTenant("alice", "", false)
	assertTenant("alice2", repotest.TenantA, true)

	require.NoError(t, s.Delete(b, "bob"))
	assertTenant("bob", "", false)
}

func TestRLSBackstop(t *testing.T) {
	s, d := newStore(t)
	a, b := repotest.Ctx(repotest.TenantA), repotest.Ctx(repotest.TenantB)
	require.NoError(t, s.Add(a, &users.User{Username: "alice", Password: "p", Groups: []string{"g"}}))
	require.NoError(t, s.Add(b, &users.User{Username: "bob", Password: "p", Groups: []string{"g"}}))
	require.NoError(t, s.UpdateGroup(a, "g", users.NewGroup("g", nil, nil, users.PermissionTunnels)))
	require.NoError(t, s.UpdateGroup(b, "g", users.NewGroup("g", nil, nil, users.PermissionVault)))

	for _, table := range []string{"users", "user_groups", "groups"} {
		var tenants []string
		require.NoError(t, postgres.InTenant(a, d.App, func(tx *sqlx.Tx) error {
			return tx.Select(&tenants, "SELECT tenant_id FROM "+table)
		}), table)
		assert.Equal(t, []string{"a"}, tenants, table)

		var n int
		require.NoError(t, d.App.Get(&n, "SELECT count(*) FROM "+table), table)
		assert.Zero(t, n, "%s without a tenant", table)
	}
}

func TestDeletingTenantCascades(t *testing.T) {
	s, d := newStore(t)
	a := repotest.Ctx(repotest.TenantA)
	require.NoError(t, s.Add(a, &users.User{Username: "alice", Password: "p", Groups: []string{"g"}}))
	require.NoError(t, s.UpdateGroup(a, "g", users.NewGroup("g", nil, nil, users.PermissionTunnels)))
	require.NoError(t, s.Add(repotest.Ctx(repotest.TenantB), &users.User{Username: "bob", Password: "p"}))

	_, err := d.Owner.Exec("DELETE FROM tenants WHERE id = 'a'")
	require.NoError(t, err)
	for _, table := range []string{"users", "user_groups", "groups", "user_logins"} {
		var n int
		require.NoError(t, d.Owner.Get(&n, "SELECT count(*) FROM "+table+" WHERE tenant_id = 'a'"), table)
		assert.Zero(t, n, table)
	}
	_, found, err := s.TenantOf(context.Background(), "bob")
	require.NoError(t, err)
	assert.True(t, found)
}

func TestStoresOSSJSON(t *testing.T) {
	s, d := newStore(t)
	a := repotest.Ctx(repotest.TenantA)
	require.NoError(t, s.UpdateGroup(a, "g", users.NewGroup("g", nil, nil, users.PermissionCommands)))
	var perms string
	var tr *string
	require.NoError(t, d.Owner.QueryRow("SELECT permissions, tunnels_restricted FROM groups WHERE tenant_id = 'a' AND name = 'g'").Scan(&perms, &tr))
	assert.JSONEq(t, `{"commands":true,"scripts":true}`, perms)
	assert.Nil(t, tr)
}

// The reserved tenants (requests whose tenant can't be resolved) never have users, whatever writes them.
func TestNoUsersInReservedTenant(t *testing.T) {
	s, d := newStore(t)
	err := s.Add(tenant.WithID(context.Background(), "_unresolved"), &users.User{Username: "someone", Password: "$2y$05$x"})
	assert.ErrorContains(t, err, "reserved")
	var n int
	require.NoError(t, d.Owner.Get(&n, "SELECT count(*) FROM users"))
	assert.Zero(t, n)
	require.NoError(t, s.Add(tenant.WithID(context.Background(), tenant.Default), &users.User{Username: "admin", Password: "$2y$05$x"}))
}
