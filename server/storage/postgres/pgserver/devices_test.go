package pgserver

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	chserver "github.com/openrport/openrport/server"
	"github.com/openrport/openrport/server/clientsauth"
	"github.com/openrport/openrport/server/storage/postgres/clientauth"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	"github.com/openrport/openrport/share/tenant"
)

func TestDeviceTenants(t *testing.T) {
	ctx := context.Background()
	d := pgtest.New(t, "a", "b", "s")
	_, err := d.Owner.Exec(`UPDATE tenants SET status = 'suspended' WHERE id = 's'; UPDATE tenants SET max_clients = 3 WHERE id = 'b'`)
	require.NoError(t, err)
	store := clientauth.New(d.App)
	for _, id := range []tenant.ID{"a", "b", "s"} {
		added, err := store.Add(tenant.WithID(ctx, id), &clientsauth.ClientAuth{ID: "auth-" + string(id), Password: "p"})
		require.NoError(t, err)
		require.True(t, added)
	}
	dt := NewDeviceTenants(d.App)

	for _, id := range []tenant.ID{"a", "b", "s"} {
		got, found, err := dt.TenantOfClientAuth(ctx, "auth-"+string(id))
		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, id, got)
	}
	_, found, err := dt.TenantOfClientAuth(ctx, "auth-x")
	require.NoError(t, err)
	assert.False(t, found)

	policy := func(id tenant.ID) chserver.DevicePolicy {
		p, err := dt.DevicePolicy(ctx, id)
		require.NoError(t, err)
		return p
	}
	assert.Equal(t, chserver.DevicePolicy{Status: "active"}, policy("a"))
	assert.Equal(t, chserver.DevicePolicy{Status: "active", Limited: true, MaxClients: 3}, policy("b"))
	assert.Equal(t, "suspended", policy("s").Status)
	assert.Equal(t, "reserved", policy("_unresolved").Status)
	assert.Equal(t, "deleted", policy("gone").Status)
	assert.Equal(t, chserver.DevicePolicy{Status: "active"}, policy(tenant.Default))
}
