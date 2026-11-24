package repotest

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	extperm "github.com/openrport/openrport/plus/capabilities/extendedpermission"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/share/tenant"
)

// UsersOptions adjusts the Users suite to the provider under test.
type UsersOptions struct {
	// OnlyTenant names the one tenant a single-tenant provider serves (the admin-defined user database serves only
	// tenant.Default). The suite then runs its round trips in that tenant, checks that TenantA is refused with
	// tenant.ErrNotSupported, and skips the isolation checks. Empty means the provider serves any tenant.
	OnlyTenant tenant.ID
}

// Users checks a users.Provider. The provider must store two-factor fields (two_fa_send_to, totp_secret) and
// support group permissions.
func Users(t *testing.T, newRepo Factory[users.Provider], opts UsersOptions) {
	home := TenantA
	if opts.OnlyTenant != "" {
		home = opts.OnlyTenant
	}
	ctx := Ctx(home)

	t.Run("RequiresTenant", func(t *testing.T) {
		assertUsersRefuse(context.Background(), t, newRepo(t), tenant.ErrMissing)
	})

	if opts.OnlyTenant != "" {
		t.Run("RefusesOtherTenants", func(t *testing.T) {
			other := TenantA
			if other == opts.OnlyTenant {
				other = TenantB
			}
			assertUsersRefuse(Ctx(other), t, newRepo(t), tenant.ErrNotSupported)
		})
	}

	t.Run("AddAndGet", func(t *testing.T) {
		p := newRepo(t)
		require.NoError(t, p.Add(ctx, &users.User{
			Username:    "alice",
			Password:    "hash-a",
			TwoFASendTo: "alice@example.com",
			TotP:        "totp-a",
			Groups:      []string{"g1", "g2"},
		}))
		u, err := p.GetByUsername(ctx, "alice")
		require.NoError(t, err)
		require.NotNil(t, u)
		assert.Equal(t, "alice", u.Username)
		assert.Equal(t, "hash-a", u.Password)
		assert.Equal(t, "alice@example.com", u.TwoFASendTo)
		assert.Equal(t, "totp-a", u.TotP)
		require.NotNil(t, u.PasswordExpired)
		assert.False(t, *u.PasswordExpired)
		assert.ElementsMatch(t, []string{"g1", "g2"}, u.Groups)

		missing, err := p.GetByUsername(ctx, "nobody")
		require.NoError(t, err)
		assert.Nil(t, missing)
	})

	t.Run("GetAllSortedWithGroups", func(t *testing.T) {
		p := newRepo(t)
		for _, u := range []*users.User{
			{Username: "carol", Password: "c", Groups: []string{"g2", "g1"}},
			{Username: "alice", Password: "a"},
			{Username: "bob", Password: "b", Groups: []string{"g1"}},
		} {
			require.NoError(t, p.Add(ctx, u))
		}
		all, err := p.GetAll(ctx)
		require.NoError(t, err)
		require.Len(t, all, 3)
		assert.Equal(t, []string{"alice", "bob", "carol"}, usernames(all))
		assert.Empty(t, all[0].Groups)
		assert.Equal(t, []string{"g1"}, all[1].Groups)
		assert.Equal(t, []string{"g1", "g2"}, all[2].Groups, "groups sorted by name")
		assert.Equal(t, "c", all[2].Password)
	})

	t.Run("UpdateChangesOnlyGivenFields", func(t *testing.T) {
		p := newRepo(t)
		require.NoError(t, p.Add(ctx, &users.User{Username: "alice", Password: "p1", TwoFASendTo: "a@x", TotP: "t1", Groups: []string{"g1"}}))

		require.NoError(t, p.Update(ctx, &users.User{Password: "p2"}, "alice"))
		u := getUser(ctx, t, p, "alice")
		assert.Equal(t, "p2", u.Password)
		assert.Equal(t, "a@x", u.TwoFASendTo)
		assert.Equal(t, "t1", u.TotP)
		assert.False(t, *u.PasswordExpired)
		assert.Equal(t, []string{"g1"}, u.Groups, "nil groups keep the membership")

		require.NoError(t, p.Update(ctx, &users.User{PasswordExpired: users.PasswordExpired(true), TwoFASendTo: "b@x", TotP: "t2"}, "alice"))
		u = getUser(ctx, t, p, "alice")
		assert.Equal(t, "p2", u.Password)
		assert.True(t, *u.PasswordExpired)
		assert.Equal(t, "b@x", u.TwoFASendTo)
		assert.Equal(t, "t2", u.TotP)

		require.NoError(t, p.Update(ctx, &users.User{Groups: []string{"g2", "g3"}}, "alice"))
		assert.ElementsMatch(t, []string{"g2", "g3"}, getUser(ctx, t, p, "alice").Groups, "groups are replaced")

		require.NoError(t, p.Update(ctx, &users.User{Groups: []string{}}, "alice"))
		assert.Empty(t, getUser(ctx, t, p, "alice").Groups, "empty groups clear the membership")

		assert.Error(t, p.Update(ctx, &users.User{Password: "x"}, ""), "an empty username to update is refused")
	})

	t.Run("UpdateRenames", func(t *testing.T) {
		p := newRepo(t)
		require.NoError(t, p.Add(ctx, &users.User{Username: "alice", Password: "p1", Groups: []string{"g1", "g2"}}))
		require.NoError(t, p.Add(ctx, &users.User{Username: "other", Password: "o", Groups: []string{"g1"}}))

		require.NoError(t, p.Update(ctx, &users.User{Username: "alice2"}, "alice"))
		gone, err := p.GetByUsername(ctx, "alice")
		require.NoError(t, err)
		assert.Nil(t, gone)
		u := getUser(ctx, t, p, "alice2")
		assert.Equal(t, "p1", u.Password)
		assert.ElementsMatch(t, []string{"g1", "g2"}, u.Groups, "the membership follows the rename")

		// rename and new groups at once
		require.NoError(t, p.Update(ctx, &users.User{Username: "alice3", Password: "p3", Groups: []string{"g3"}}, "alice2"))
		u = getUser(ctx, t, p, "alice3")
		assert.Equal(t, "p3", u.Password)
		assert.Equal(t, []string{"g3"}, u.Groups)

		// renaming to the same name changes nothing else
		require.NoError(t, p.Update(ctx, &users.User{Username: "alice3", Password: "p4"}, "alice3"))
		assert.Equal(t, "p4", getUser(ctx, t, p, "alice3").Password)

		assert.Equal(t, []string{"g1"}, getUser(ctx, t, p, "other").Groups, "other users are untouched")
	})

	t.Run("Delete", func(t *testing.T) {
		p := newRepo(t)
		require.NoError(t, p.Add(ctx, &users.User{Username: "alice", Password: "p", Groups: []string{"only-alice"}}))
		require.NoError(t, p.Add(ctx, &users.User{Username: "bob", Password: "p", Groups: []string{"shared"}}))

		require.NoError(t, p.Delete(ctx, "alice"))
		gone, err := p.GetByUsername(ctx, "alice")
		require.NoError(t, err)
		assert.Nil(t, gone)
		groups, err := p.ListGroups(ctx)
		require.NoError(t, err)
		assert.Equal(t, []string{"shared"}, groupNames(groups), "the deleted user's membership is gone")
		assert.NotNil(t, getUser(ctx, t, p, "bob"))

		assert.NoError(t, p.Delete(ctx, "nobody"), "deleting an unknown user is not an error")
	})

	t.Run("Groups", func(t *testing.T) {
		p := newRepo(t)
		require.True(t, p.SupportsGroupPermissions())
		require.NoError(t, p.Add(ctx, &users.User{Username: "alice", Password: "p", Groups: []string{"members-only", "g1"}}))

		tr := &extperm.PermissionParams{"local": []interface{}{"^22$"}, "scheme": []interface{}{"ssh"}}
		cr := &extperm.PermissionParams{"allow": []interface{}{"^ls"}, "deny": []interface{}{"rm"}, "is_sudo": false}
		g1 := users.NewGroup("g1", tr, cr, users.PermissionCommands, users.PermissionTunnels)
		require.NoError(t, p.UpdateGroup(ctx, "g1", g1))
		require.NoError(t, p.UpdateGroup(ctx, "details-only", users.Group{Name: "ignored", Permissions: users.NewPermissions(users.PermissionVault)}))

		got, err := p.GetGroup(ctx, "g1")
		require.NoError(t, err)
		assert.Equal(t, g1, got)
		got, err = p.GetGroup(ctx, "details-only")
		require.NoError(t, err)
		assert.Equal(t, users.NewGroup("details-only", nil, nil, users.PermissionVault), got, "the name argument wins")

		got, err = p.GetGroup(ctx, "unknown")
		require.NoError(t, err)
		assert.Equal(t, users.NewGroup("unknown", nil, nil), got, "an unknown group has no permissions")
		got, err = p.GetGroup(ctx, users.Administrators)
		require.NoError(t, err)
		assert.Equal(t, users.AdministratorsGroup, got)

		list, err := p.ListGroups(ctx)
		require.NoError(t, err)
		assert.ElementsMatch(t, []users.Group{
			g1,
			users.NewGroup("details-only", nil, nil, users.PermissionVault),
			users.NewGroup("members-only", nil, nil),
		}, list, "groups with details and groups that exist only through a membership")

		// updating replaces the stored group, restrictions included
		g1b := users.NewGroup("g1", nil, nil, users.PermissionMonitoring)
		require.NoError(t, p.UpdateGroup(ctx, "g1", g1b))
		got, err = p.GetGroup(ctx, "g1")
		require.NoError(t, err)
		assert.Equal(t, g1b, got)

		// deleting removes the details and the membership
		require.NoError(t, p.DeleteGroup(ctx, "g1"))
		got, err = p.GetGroup(ctx, "g1")
		require.NoError(t, err)
		assert.Equal(t, users.NewGroup("g1", nil, nil), got)
		assert.Equal(t, []string{"members-only"}, getUser(ctx, t, p, "alice").Groups)
		list, err = p.ListGroups(ctx)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"details-only", "members-only"}, groupNames(list))
	})

	if opts.OnlyTenant != "" {
		return
	}

	t.Run("IsolatesTenants", func(t *testing.T) {
		p := newRepo(t)
		a, b := Ctx(TenantA), Ctx(TenantB)
		// usernames may be unique across tenants, so each tenant has its own; group names are per tenant
		require.NoError(t, p.Add(a, &users.User{Username: "alice", Password: "pa", Groups: []string{"g"}}))
		require.NoError(t, p.Add(b, &users.User{Username: "bob", Password: "pb", Groups: []string{"g"}}))
		require.NoError(t, p.UpdateGroup(a, "g", users.NewGroup("g", nil, nil, users.PermissionTunnels)))
		require.NoError(t, p.UpdateGroup(b, "g", users.NewGroup("g", nil, nil, users.PermissionVault)))

		u, err := p.GetByUsername(b, "alice")
		require.NoError(t, err)
		assert.Nil(t, u)
		all, err := p.GetAll(a)
		require.NoError(t, err)
		assert.Equal(t, []string{"alice"}, usernames(all))
		all, err = p.GetAll(b)
		require.NoError(t, err)
		assert.Equal(t, []string{"bob"}, usernames(all))

		ga, err := p.GetGroup(a, "g")
		require.NoError(t, err)
		assert.True(t, ga.Permissions.Has(users.PermissionTunnels))
		assert.False(t, ga.Permissions.Has(users.PermissionVault))
		list, err := p.ListGroups(b)
		require.NoError(t, err)
		assert.Equal(t, []users.Group{users.NewGroup("g", nil, nil, users.PermissionVault)}, list)

		// b can't change or delete a's user or group
		_ = p.Update(b, &users.User{Username: "mallory", Password: "x", Groups: []string{"evil"}}, "alice")
		_ = p.Delete(b, "alice")
		require.NoError(t, p.DeleteGroup(b, "g"))
		u = getUser(a, t, p, "alice")
		assert.Equal(t, "pa", u.Password)
		assert.Equal(t, []string{"g"}, u.Groups)
		ga, err = p.GetGroup(a, "g")
		require.NoError(t, err)
		assert.True(t, ga.Permissions.Has(users.PermissionTunnels))
		u, err = p.GetByUsername(b, "mallory")
		require.NoError(t, err)
		assert.Nil(t, u)

		// b's own data went
		assert.Empty(t, getUser(b, t, p, "bob").Groups)
	})
}

func assertUsersRefuse(ctx context.Context, t *testing.T, p users.Provider, want error) {
	t.Helper()
	_, err := p.GetAll(ctx)
	assert.ErrorIs(t, err, want, "GetAll")
	_, err = p.GetByUsername(ctx, "alice")
	assert.ErrorIs(t, err, want, "GetByUsername")
	_, err = p.ListGroups(ctx)
	assert.ErrorIs(t, err, want, "ListGroups")
	_, err = p.GetGroup(ctx, "g")
	assert.ErrorIs(t, err, want, "GetGroup")
	assert.ErrorIs(t, p.UpdateGroup(ctx, "g", users.Group{Name: "g"}), want, "UpdateGroup")
	assert.ErrorIs(t, p.DeleteGroup(ctx, "g"), want, "DeleteGroup")
	assert.ErrorIs(t, p.Add(ctx, &users.User{Username: "alice", Password: "x"}), want, "Add")
	assert.ErrorIs(t, p.Update(ctx, &users.User{Password: "x"}, "alice"), want, "Update")
	assert.ErrorIs(t, p.Delete(ctx, "alice"), want, "Delete")
}

func getUser(ctx context.Context, t *testing.T, p users.Provider, username string) *users.User {
	t.Helper()
	u, err := p.GetByUsername(ctx, username)
	require.NoError(t, err)
	require.NotNil(t, u, "user %q", username)
	sort.Strings(u.Groups)
	return u
}

func usernames(us []*users.User) []string {
	out := make([]string, 0, len(us))
	for _, u := range us {
		out = append(out, u.Username)
	}
	return out
}

func groupNames(gs []users.Group) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Name)
	}
	sort.Strings(out)
	return out
}
