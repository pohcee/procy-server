package clients

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
	"github.com/openrport/openrport/share/test"
)

func TestSyncSharedTakesConnectionStateFromRegistry(t *testing.T) {
	log := logger.NewLogger("test", logger.LogOutput{}, logger.LogLevelError)
	past := time.Now().Add(-time.Hour)
	held := &clientdata.Client{ID: "held", Name: "held-here", Connection: test.NewConnMock()}
	stale := &clientdata.Client{ID: "stale", Name: "old name", DisconnectedAt: &past}
	dropped := &clientdata.Client{ID: "dropped", DisconnectedAt: &past}
	repo := NewClientRepository([]*clientdata.Client{held, stale, dropped}, nil, log)

	stored := []*clientdata.Client{
		{ID: "held", Name: "stored name"},                      // this instance holds it: keeps its object
		{ID: "stale", Name: "new name", DisconnectedAt: &past}, // connected elsewhere per the registry
		{ID: "fresh"},                        // stored as connected, no live session
		{ID: "other", DisconnectedAt: &past}, // new, disconnected
	}
	repo.SyncShared(tenant.Default, stored, map[string]bool{"stale": true, "held": true})

	ctx := tenant.WithID(context.Background(), tenant.Default)
	get := func(id string) *clientdata.Client {
		c, err := repo.GetByID(ctx, id)
		require.NoError(t, err)
		return c
	}
	assert.Same(t, held, get("held"))
	assert.Equal(t, "held-here", get("held").GetName())
	assert.Equal(t, "new name", get("stale").GetName())
	assert.True(t, get("stale").IsConnected())
	assert.False(t, get("fresh").IsConnected(), "no live session: disconnected whatever the record says")
	assert.False(t, get("other").IsConnected())
	assert.Nil(t, get("dropped"))

	// a tenant that isn't loaded is left alone
	repo.SyncShared("b", stored, nil)
	assert.ElementsMatch(t, []tenant.ID{tenant.Default}, repo.LoadedTenants())
}
