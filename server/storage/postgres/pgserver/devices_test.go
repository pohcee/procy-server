package pgserver

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	chserver "github.com/pohcee/procy-server/server"
	"github.com/pohcee/procy-server/server/clientsauth"
	"github.com/pohcee/procy-server/server/storage/postgres/clientauth"
	"github.com/pohcee/procy-server/server/storage/postgres/pgtest"
	"github.com/pohcee/procy-server/share/tenant"
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
	dt := NewDeviceTenants(d.App, nil)

	for _, id := range []tenant.ID{"a", "b", "s"} {
		got, found, err := dt.TenantOfClientAuth(ctx, "auth-"+string(id))
		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, id, got)
	}
	_, found, err := dt.TenantOfClientAuth(ctx, "auth-x")
	require.NoError(t, err)
	assert.False(t, found)

	admission := func(id tenant.ID) chserver.Admission {
		a, err := dt.AdmitClient(ctx, id, "c1")
		require.NoError(t, err)
		return a
	}
	assert.Equal(t, chserver.Admission{Status: "active"}, admission("a"))
	assert.Equal(t, chserver.Admission{Status: "active", Clients: 1, MaxClients: 3}, admission("b"))
	assert.Equal(t, chserver.Admission{Refused: chserver.RefusedStatus, Status: "suspended"}, admission("s"))
	assert.Equal(t, chserver.Admission{Refused: chserver.RefusedStatus, Status: "reserved"}, admission("_unresolved"))
	assert.Equal(t, chserver.Admission{Refused: chserver.RefusedStatus, Status: "deleted"}, admission("gone"))
	assert.Equal(t, chserver.Admission{Status: "active"}, admission(tenant.Default))

	tids, err := dt.Tenants(ctx)
	require.NoError(t, err)
	assert.Equal(t, []tenant.ID{"a", "b", tenant.Default, "s"}, tids, "every tenant but the reserved ones")
}
