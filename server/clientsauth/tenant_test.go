package clientsauth

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/tenant"
)

var defaultCtx = tenant.WithID(context.Background(), tenant.Default)

func providersForTenantTest(t *testing.T) map[string]Provider {
	c := &ClientAuth{ID: "c1", Password: "pswd"}
	return map[string]Provider{
		"file":     NewMockFileProvider([]*ClientAuth{c}, t),
		"database": NewDatabaseMockProvider([]*ClientAuth{c}, t),
		"single":   NewSingleProvider(c.ID, c.Password),
	}
}

func TestProvidersServeDefaultTenant(t *testing.T) {
	for name, p := range providersForTenantTest(t) {
		t.Run(name, func(t *testing.T) {
			got, err := p.Get(defaultCtx, "c1")
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, "pswd", got.Password)

			list, count, err := p.GetFiltered(defaultCtx, &query.ListOptions{Pagination: query.NewPagination(5, 0)})
			require.NoError(t, err)
			assert.Equal(t, 1, count)
			assert.Len(t, list, 1)
		})
	}
}

func TestProvidersRejectOtherTenants(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"missing tenant", context.Background(), tenant.ErrMissing},
		{"other tenant", tenant.WithID(context.Background(), "acme"), tenant.ErrNotSupported},
	} {
		for name, p := range providersForTenantTest(t) {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				_, err := p.Get(tc.ctx, "c1")
				assert.ErrorIs(t, err, tc.want)
				_, _, err = p.GetFiltered(tc.ctx, &query.ListOptions{Pagination: query.NewPagination(5, 0)})
				assert.ErrorIs(t, err, tc.want)
				_, err = p.Add(tc.ctx, &ClientAuth{ID: "c2", Password: "x"})
				assert.ErrorIs(t, err, tc.want)
				assert.ErrorIs(t, p.Delete(tc.ctx, "c1"), tc.want)
			})
		}
	}
}

func TestFileProviderCacheKeyIncludesTenant(t *testing.T) {
	p := NewMockFileProvider([]*ClientAuth{{ID: "c1", Password: "pswd"}}, t)
	assert.NotEqual(t, p.CacheKey(tenant.Default, "c1"), p.CacheKey("acme", "c1"))

	_, err := p.Get(defaultCtx, "c1")
	require.NoError(t, err)
	_, found := p.cache.Get(p.CacheKey(tenant.Default, "c1"))
	assert.True(t, found)
}
