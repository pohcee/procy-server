package repotest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api/authorization"
	"github.com/openrport/openrport/share/tenant"
)

// APITokens checks an authorization.DbProvider (API tokens).
func APITokens(t *testing.T, newRepo Factory[authorization.DbProvider]) {
	a, b := Ctx(TenantA), Ctx(TenantB)

	t.Run("RequiresTenant", func(t *testing.T) {
		p := newRepo(t)
		ctx := context.Background()
		_, err := p.Get(ctx, "u", "prefix01")
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = p.GetByName(ctx, "u", "n")
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = p.GetAll(ctx, "u")
		assert.ErrorIs(t, err, tenant.ErrMissing)
		assert.ErrorIs(t, p.Save(ctx, apiToken("u", "prefix01", "n", "t")), tenant.ErrMissing)
		assert.ErrorIs(t, p.Delete(ctx, "u", "prefix01"), tenant.ErrMissing)
	})

	t.Run("SaveAndGet", func(t *testing.T) {
		p := newRepo(t)
		created := time.Date(2001, 1, 1, 1, 0, 0, 0, time.UTC)
		expires := time.Date(2031, 1, 1, 2, 0, 0, 0, time.UTC)
		tk := &authorization.APIToken{
			Username: "u", Prefix: "prefix01", Name: "first", Scope: authorization.APITokenReadWrite,
			Token: "hash", CreatedAt: &created, ExpiresAt: &expires,
		}
		require.NoError(t, p.Save(a, tk))
		assert.Equal(t, TenantA, tk.TenantID, "Save sets the tenant of the context")

		got, err := p.Get(a, "u", "prefix01")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, TenantA, got.TenantID)
		assert.Equal(t, "u", got.Username)
		assert.Equal(t, "prefix01", got.Prefix)
		assert.Equal(t, "first", got.Name)
		assert.Equal(t, authorization.APITokenReadWrite, got.Scope)
		assert.Equal(t, "hash", got.Token)
		assertSameTime(t, created, got.CreatedAt)
		assertSameTime(t, expires, got.ExpiresAt)

		byName, err := p.GetByName(a, "u", "first")
		require.NoError(t, err)
		require.NotNil(t, byName)
		assert.Equal(t, "prefix01", byName.Prefix)

		none, err := p.Get(a, "u", "prefix02")
		require.NoError(t, err)
		assert.Nil(t, none)
		none, err = p.Get(a, "other", "prefix01")
		require.NoError(t, err)
		assert.Nil(t, none)
		none, err = p.GetByName(a, "u", "unknown")
		require.NoError(t, err)
		assert.Nil(t, none)
	})

	t.Run("SaveDefaultsCreatedAtAndNoExpiry", func(t *testing.T) {
		p := newRepo(t)
		before := time.Now().Add(-2 * time.Second)
		require.NoError(t, p.Save(a, apiToken("u", "prefix01", "n", "t")))
		got, err := p.Get(a, "u", "prefix01")
		require.NoError(t, err)
		require.NotNil(t, got)
		require.NotNil(t, got.CreatedAt)
		assert.WithinDuration(t, before, *got.CreatedAt, time.Minute)
		assert.Nil(t, got.ExpiresAt)
	})

	t.Run("SaveUpdatesNameAndExpiry", func(t *testing.T) {
		p := newRepo(t)
		expires := time.Date(2031, 1, 1, 2, 0, 0, 0, time.UTC)
		tk := apiToken("u", "prefix01", "first", "hash")
		tk.ExpiresAt = &expires
		require.NoError(t, p.Save(a, tk))

		// only the name
		require.NoError(t, p.Save(a, &authorization.APIToken{Username: "u", Prefix: "prefix01", Name: "renamed"}))
		got, err := p.Get(a, "u", "prefix01")
		require.NoError(t, err)
		assert.Equal(t, "renamed", got.Name)
		assertSameTime(t, expires, got.ExpiresAt)
		assert.Equal(t, "hash", got.Token, "the token can't be changed")
		assert.Equal(t, authorization.APITokenRead, got.Scope, "the scope can't be changed")

		// only the expiry
		later := expires.Add(24 * time.Hour)
		require.NoError(t, p.Save(a, &authorization.APIToken{Username: "u", Prefix: "prefix01", ExpiresAt: &later}))
		got, err = p.Get(a, "u", "prefix01")
		require.NoError(t, err)
		assert.Equal(t, "renamed", got.Name)
		assertSameTime(t, later, got.ExpiresAt)
	})

	t.Run("GetAllOfUser", func(t *testing.T) {
		p := newRepo(t)
		require.NoError(t, p.Save(a, apiToken("u", "prefix01", "n1", "t1")))
		require.NoError(t, p.Save(a, apiToken("u", "prefix02", "n2", "t2")))
		require.NoError(t, p.Save(a, apiToken("v", "prefix03", "n3", "t3")))
		all, err := p.GetAll(a, "u")
		require.NoError(t, err)
		var prefixes []string
		for _, tk := range all {
			prefixes = append(prefixes, tk.Prefix)
			assert.Equal(t, TenantA, tk.TenantID)
		}
		assert.ElementsMatch(t, []string{"prefix01", "prefix02"}, prefixes)

		none, err := p.GetAll(a, "nobody")
		require.NoError(t, err)
		assert.Empty(t, none)
	})

	t.Run("NameUniquePerUser", func(t *testing.T) {
		p := newRepo(t)
		require.NoError(t, p.Save(a, apiToken("u", "prefix01", "n", "t1")))
		assert.Error(t, p.Save(a, apiToken("u", "prefix02", "n", "t2")), "a user's token names are unique")
		assert.NoError(t, p.Save(a, apiToken("v", "prefix02", "n", "t2")), "other users may use the name")
	})

	t.Run("Delete", func(t *testing.T) {
		p := newRepo(t)
		require.NoError(t, p.Save(a, apiToken("u", "prefix01", "n1", "t1")))
		require.NoError(t, p.Save(a, apiToken("u", "prefix02", "n2", "t2")))
		require.NoError(t, p.Delete(a, "u", "prefix01"))
		gone, err := p.Get(a, "u", "prefix01")
		require.NoError(t, err)
		assert.Nil(t, gone)
		kept, err := p.Get(a, "u", "prefix02")
		require.NoError(t, err)
		assert.NotNil(t, kept)

		assert.Error(t, p.Delete(a, "u", "prefix01"), "deleting a missing token fails")
		assert.Error(t, p.Delete(a, "v", "prefix02"), "the username must match")
	})

	t.Run("IsolatesTenants", func(t *testing.T) {
		p := newRepo(t)
		// same username, prefix and name in both tenants
		require.NoError(t, p.Save(a, apiToken("u", "prefix01", "n", "token-a")))
		require.NoError(t, p.Save(b, apiToken("u", "prefix01", "n", "token-b")))

		got, err := p.Get(a, "u", "prefix01")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "token-a", got.Token)
		byName, err := p.GetByName(b, "u", "n")
		require.NoError(t, err)
		require.NotNil(t, byName)
		assert.Equal(t, "token-b", byName.Token)
		all, err := p.GetAll(a, "u")
		require.NoError(t, err)
		require.Len(t, all, 1)
		assert.Equal(t, "token-a", all[0].Token)

		require.NoError(t, p.Save(a, &authorization.APIToken{Username: "u", Prefix: "prefix01", Name: "renamed"}))
		still, err := p.Get(b, "u", "prefix01")
		require.NoError(t, err)
		assert.Equal(t, "n", still.Name)

		require.NoError(t, p.Delete(a, "u", "prefix01"))
		still, err = p.Get(b, "u", "prefix01")
		require.NoError(t, err)
		assert.NotNil(t, still)
		assert.Error(t, p.Delete(a, "u", "prefix01"), "b's token is not found from a")

		// a token only in b
		require.NoError(t, p.Save(b, apiToken("w", "prefix09", "only-b", "t")))
		none, err := p.Get(a, "w", "prefix09")
		require.NoError(t, err)
		assert.Nil(t, none)
		none, err = p.GetByName(a, "w", "only-b")
		require.NoError(t, err)
		assert.Nil(t, none)
	})

	t.Run("IgnoresTenantOnStruct", func(t *testing.T) {
		p := newRepo(t)
		tk := apiToken("u", "prefix01", "n", "t")
		tk.TenantID = TenantB
		require.NoError(t, p.Save(a, tk))
		got, err := p.Get(b, "u", "prefix01")
		require.NoError(t, err)
		assert.Nil(t, got)
		got, err = p.Get(a, "u", "prefix01")
		require.NoError(t, err)
		assert.NotNil(t, got)
	})
}

func apiToken(username, prefix, name, token string) *authorization.APIToken {
	return &authorization.APIToken{Username: username, Prefix: prefix, Name: name, Scope: authorization.APITokenRead, Token: token}
}

// assertSameTime checks that got is the instant want, whatever its location; storage may keep less than
// nanosecond precision, so the suites use whole milliseconds.
func assertSameTime(t *testing.T, want time.Time, got *time.Time) {
	t.Helper()
	if assert.NotNil(t, got) {
		assert.True(t, want.Equal(*got), "want %s, got %s", want, *got)
	}
}
