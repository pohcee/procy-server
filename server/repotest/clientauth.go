package repotest

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/clientsauth"
	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/tenant"
)

// ClientAuthOptions adjusts the ClientAuth suite to the provider under test.
type ClientAuthOptions struct {
	// OnlyTenant names the one tenant a single-tenant provider serves (the admin-defined client auth table serves
	// only tenant.Default). The suite then runs its round trips in that tenant, checks that another tenant is
	// refused with tenant.ErrNotSupported, and skips the isolation checks. Empty means any tenant is served.
	OnlyTenant tenant.ID
	// Hashed is set for a provider that stores only password hashes (a clientsauth.Verifier): Get and GetFiltered
	// then return no password, and the suite checks passwords with clientsauth.Verify.
	Hashed bool
}

// assertPassword checks that got (as Get or GetFiltered returned it) has the password pw.
func assertPassword(ctx context.Context, t *testing.T, p clientsauth.Provider, opts ClientAuthOptions, got *clientsauth.ClientAuth, pw string) {
	t.Helper()
	if !opts.Hashed {
		assert.Equal(t, pw, got.Password)
		return
	}
	assert.Empty(t, got.Password, "a hashing provider never returns the password or its hash")
	ok, err := clientsauth.Verify(ctx, p, got.ID, []byte(pw))
	require.NoError(t, err)
	assert.True(t, ok, "the password verifies")
	ok, err = clientsauth.Verify(ctx, p, got.ID, []byte(pw+"x"))
	require.NoError(t, err)
	assert.False(t, ok, "another password doesn't")
}

// ClientAuth checks a writeable clientsauth.Provider.
func ClientAuth(t *testing.T, newRepo Factory[clientsauth.Provider], opts ClientAuthOptions) {
	home := TenantA
	if opts.OnlyTenant != "" {
		home = opts.OnlyTenant
	}
	ctx := Ctx(home)

	t.Run("RequiresTenant", func(t *testing.T) {
		assertClientAuthRefuses(context.Background(), t, newRepo(t), tenant.ErrMissing)
	})

	if opts.OnlyTenant != "" {
		t.Run("RefusesOtherTenants", func(t *testing.T) {
			other := TenantA
			if other == opts.OnlyTenant {
				other = TenantB
			}
			assertClientAuthRefuses(Ctx(other), t, newRepo(t), tenant.ErrNotSupported)
		})
	}

	t.Run("AddGetDelete", func(t *testing.T) {
		p := newRepo(t)
		require.True(t, p.IsWriteable())
		added, err := p.Add(ctx, &clientsauth.ClientAuth{ID: "c1", Password: "pw1"})
		require.NoError(t, err)
		assert.True(t, added)

		got, err := p.Get(ctx, "c1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "c1", got.ID)
		assertPassword(ctx, t, p, opts, got, "pw1")

		added, err = p.Add(ctx, &clientsauth.ClientAuth{ID: "c1", Password: "other"})
		require.NoError(t, err)
		assert.False(t, added, "an existing id is reported, not replaced")
		got, err = p.Get(ctx, "c1")
		require.NoError(t, err)
		assertPassword(ctx, t, p, opts, got, "pw1")

		missing, err := p.Get(ctx, "nope")
		require.NoError(t, err)
		assert.Nil(t, missing)

		require.NoError(t, p.Delete(ctx, "c1"))
		gone, err := p.Get(ctx, "c1")
		require.NoError(t, err)
		assert.Nil(t, gone)
		assert.NoError(t, p.Delete(ctx, "c1"), "deleting a missing id is not an error")
	})

	t.Run("GetFiltered", func(t *testing.T) {
		p := newRepo(t)
		for i := 5; i >= 1; i-- {
			added, err := p.Add(ctx, &clientsauth.ClientAuth{ID: fmt.Sprintf("client-%d", i), Password: fmt.Sprintf("pw%d", i)})
			require.NoError(t, err)
			require.True(t, added)
		}
		added, err := p.Add(ctx, &clientsauth.ClientAuth{ID: "Other", Password: "x"})
		require.NoError(t, err)
		require.True(t, added)

		list, count, err := p.GetFiltered(ctx, &query.ListOptions{Pagination: query.NewPagination(50, 0)})
		require.NoError(t, err)
		assert.Equal(t, 6, count)
		assert.Equal(t, []string{"Other", "client-1", "client-2", "client-3", "client-4", "client-5"}, clientAuthIDs(list), "sorted by id")
		assertPassword(ctx, t, p, opts, list[1], "pw1")

		list, count, err = p.GetFiltered(ctx, &query.ListOptions{Pagination: query.NewPagination(2, 2)})
		require.NoError(t, err)
		assert.Equal(t, 6, count, "the count ignores the pagination")
		assert.Equal(t, []string{"client-2", "client-3"}, clientAuthIDs(list))

		list, count, err = p.GetFiltered(ctx, &query.ListOptions{
			Filters:    []query.FilterOption{idFilter("client-3")},
			Pagination: query.NewPagination(50, 0),
		})
		require.NoError(t, err)
		assert.Equal(t, 1, count)
		assert.Equal(t, []string{"client-3"}, clientAuthIDs(list))

		list, count, err = p.GetFiltered(ctx, &query.ListOptions{
			Filters:    []query.FilterOption{idFilter("client-1", "client-4")},
			Pagination: query.NewPagination(50, 0),
		})
		require.NoError(t, err)
		assert.Equal(t, 2, count, "values of one filter are or-ed")
		assert.Equal(t, []string{"client-1", "client-4"}, clientAuthIDs(list))

		list, count, err = p.GetFiltered(ctx, &query.ListOptions{
			Filters:    []query.FilterOption{idFilter("CLIENT-*")},
			Pagination: query.NewPagination(3, 1),
		})
		require.NoError(t, err)
		assert.Equal(t, 5, count, "a wildcard matches case-insensitively")
		assert.Equal(t, []string{"client-2", "client-3", "client-4"}, clientAuthIDs(list))

		list, count, err = p.GetFiltered(ctx, &query.ListOptions{
			Filters:    []query.FilterOption{idFilter("none*")},
			Pagination: query.NewPagination(50, 0),
		})
		require.NoError(t, err)
		assert.Zero(t, count)
		assert.Empty(t, list)
	})

	if opts.OnlyTenant != "" {
		return
	}

	t.Run("IsolatesTenants", func(t *testing.T) {
		p := newRepo(t)
		a, b := Ctx(TenantA), Ctx(TenantB)
		// ids may be unique across tenants, so each tenant has its own
		for _, c := range []struct {
			ctx context.Context
			id  string
		}{{a, "a1"}, {a, "a2"}, {b, "b1"}} {
			added, err := p.Add(c.ctx, &clientsauth.ClientAuth{ID: c.id, Password: "pw-" + c.id})
			require.NoError(t, err)
			require.True(t, added)
		}

		got, err := p.Get(b, "a1")
		require.NoError(t, err)
		assert.Nil(t, got)
		list, count, err := p.GetFiltered(b, &query.ListOptions{Pagination: query.NewPagination(50, 0)})
		require.NoError(t, err)
		assert.Equal(t, 1, count)
		assert.Equal(t, []string{"b1"}, clientAuthIDs(list))
		list, count, err = p.GetFiltered(a, &query.ListOptions{
			Filters:    []query.FilterOption{idFilter("*1")},
			Pagination: query.NewPagination(50, 0),
		})
		require.NoError(t, err)
		assert.Equal(t, 1, count)
		assert.Equal(t, []string{"a1"}, clientAuthIDs(list))

		require.NoError(t, p.Delete(b, "a1"))
		got, err = p.Get(a, "a1")
		require.NoError(t, err)
		assert.NotNil(t, got, "b can't delete a's client auth")
	})
}

func assertClientAuthRefuses(ctx context.Context, t *testing.T, p clientsauth.Provider, want error) {
	t.Helper()
	_, err := p.Get(ctx, "c1")
	assert.ErrorIs(t, err, want, "Get")
	_, _, err = p.GetFiltered(ctx, &query.ListOptions{Pagination: query.NewPagination(5, 0)})
	assert.ErrorIs(t, err, want, "GetFiltered")
	_, err = p.Add(ctx, &clientsauth.ClientAuth{ID: "c1", Password: "x"})
	assert.ErrorIs(t, err, want, "Add")
	assert.ErrorIs(t, p.Delete(ctx, "c1"), want, "Delete")
}

func idFilter(values ...string) query.FilterOption {
	return query.FilterOption{Column: []string{"id"}, Operator: query.FilterOperatorTypeEQ, Values: values}
}

func clientAuthIDs(list []*clientsauth.ClientAuth) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.ID)
	}
	return out
}
