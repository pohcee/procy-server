package storedtunnels

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/migration/clients"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/random"
	"github.com/openrport/openrport/share/tenant"
)

var DataSourceOptions = sqlite.DataSourceOptions{WALEnabled: false}

func ctxFor(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

func TestStoredTunnels(t *testing.T) {
	ctx := ctxFor(tenant.Default)
	client1, err := random.UUID4()
	require.NoError(t, err)
	client2, err := random.UUID4()
	require.NoError(t, err)
	db, err := sqlite.New(":memory:", clients.FS, DataSourceOptions)
	require.NoError(t, err)
	tunnel := &StoredTunnel{}
	options := &query.ListOptions{}

	manager := New(db)

	// no results initially
	results, err := manager.List(ctx, options, client1)
	require.NoError(t, err)
	assert.Equal(t, 0, results.Meta.Count)

	_, err = manager.Create(ctx, client1, tunnel)
	require.NoError(t, err)

	// client1 has one stored tunnel
	results, err = manager.List(ctx, options, client1)
	require.NoError(t, err)
	assert.Equal(t, 1, results.Meta.Count)

	// client2 has no stored tunnels
	results, err = manager.List(ctx, options, client2)
	require.NoError(t, err)
	assert.Equal(t, 0, results.Meta.Count)

	err = manager.Delete(ctx, client1, tunnel.ID)
	require.NoError(t, err)

	// client1 doesn't have any stored tunnel anymore
	results, err = manager.List(ctx, options, client1)
	require.NoError(t, err)
	assert.Equal(t, 0, results.Meta.Count)
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	db, err := sqlite.New(":memory:", clients.FS, DataSourceOptions)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return New(db)
}

func TestStoredTunnelsRequireTenant(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	_, err := m.List(ctx, &query.ListOptions{}, "c1")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = m.provider.Count(ctx, "c1", &query.ListOptions{})
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = m.Create(ctx, "c1", &StoredTunnel{})
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = m.Update(ctx, "c1", &StoredTunnel{ID: "t1"})
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.ErrorIs(t, m.Delete(ctx, "c1", "t1"), tenant.ErrMissing)
}

func TestStoredTunnelsIsolateTenants(t *testing.T) {
	m := newTestManager(t)
	a, b := ctxFor("a"), ctxFor("b")

	ta, err := m.Create(a, "c1", &StoredTunnel{Name: "from a"})
	require.NoError(t, err)
	// the same id in another tenant does not collide
	require.NoError(t, m.provider.Insert(b, &StoredTunnel{ID: ta.ID, ClientID: "c1", Name: "from b"}))

	list := func(ctx context.Context) []*StoredTunnel {
		res, err := m.List(ctx, &query.ListOptions{}, "c1")
		require.NoError(t, err)
		require.Equal(t, len(res.Data.([]*StoredTunnel)), res.Meta.Count)
		return res.Data.([]*StoredTunnel)
	}
	require.Len(t, list(a), 1)
	assert.Equal(t, "from a", list(a)[0].Name)
	require.Len(t, list(b), 1)
	assert.Equal(t, "from b", list(b)[0].Name)

	_, err = m.Update(a, "c1", &StoredTunnel{ID: ta.ID, Name: "a updated"})
	require.NoError(t, err)
	assert.Equal(t, "a updated", list(a)[0].Name)
	assert.Equal(t, "from b", list(b)[0].Name)

	require.NoError(t, m.Delete(a, "c1", ta.ID))
	assert.Empty(t, list(a))
	assert.Len(t, list(b), 1)
}

func TestStoredTunnelCreateIgnoresTenantOnStruct(t *testing.T) {
	m := newTestManager(t)
	_, err := m.Create(ctxFor("a"), "c1", &StoredTunnel{TenantID: "b"})
	require.NoError(t, err)
	res, err := m.List(ctxFor("b"), &query.ListOptions{}, "c1")
	require.NoError(t, err)
	assert.Zero(t, res.Meta.Count)
}

func TestStoredTunnelTenantIDNotInJSON(t *testing.T) {
	b, err := json.Marshal(&StoredTunnel{ID: "t1", TenantID: "a"})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "tenant")
}
