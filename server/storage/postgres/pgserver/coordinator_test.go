package pgserver

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

func testLog() *logger.Logger {
	return logger.NewLogger("test", logger.LogOutput{}, logger.LogLevelError)
}

func newCoordinator(t *testing.T, d *pgtest.DB, node string) *Coordinator {
	t.Helper()
	c, err := NewCoordinator(context.Background(), d.App, node, testLog())
	require.NoError(t, err)
	return c
}

func TestCoordinatorLease(t *testing.T) {
	d := pgtest.New(t, "a")
	ctx := context.Background()
	a, b := newCoordinator(t, d, "node-a"), newCoordinator(t, d, "node-b")

	ok, err := a.Lease(ctx, "jobs-cleanup", "a", time.Hour)
	require.NoError(t, err)
	assert.True(t, ok, "free lease taken")
	ok, err = b.Lease(ctx, "jobs-cleanup", "a", time.Hour)
	require.NoError(t, err)
	assert.False(t, ok, "held by a")
	ok, err = a.Lease(ctx, "jobs-cleanup", "a", time.Hour)
	require.NoError(t, err)
	assert.True(t, ok, "renewed by its holder")
	ok, err = b.Lease(ctx, "jobs-cleanup", "default", time.Hour)
	require.NoError(t, err)
	assert.True(t, ok, "leases are per tenant")

	// a stops renewing: once expired b takes it
	ok, err = a.Lease(ctx, "short", "a", 200*time.Millisecond)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = b.Lease(ctx, "short", "a", time.Hour)
	require.NoError(t, err)
	assert.False(t, ok)
	time.Sleep(300 * time.Millisecond)
	ok, err = b.Lease(ctx, "short", "a", time.Hour)
	require.NoError(t, err)
	assert.True(t, ok, "taken over after expiry")
	ok, err = a.Lease(ctx, "short", "a", time.Hour)
	require.NoError(t, err)
	assert.False(t, ok, "the old holder doesn't get it back")

	same := newCoordinator(t, d, "node-a")
	ok, err = same.Lease(ctx, "jobs-cleanup", "a", time.Hour)
	require.NoError(t, err)
	assert.False(t, ok, "another process with the same node id is another holder")
}

func TestCoordinatorClaimRunOnce(t *testing.T) {
	d := pgtest.New(t, "a", "b")
	a, b := newCoordinator(t, d, "node-a"), newCoordinator(t, d, "node-b")
	fire := time.Date(2026, 11, 24, 10, 5, 0, 0, time.UTC)
	ctxA := tenant.WithID(context.Background(), "a")

	ok, err := a.ClaimRun(ctxA, "s1", fire)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = b.ClaimRun(ctxA, "s1", fire.In(time.FixedZone("x", 3600)))
	require.NoError(t, err)
	assert.False(t, ok, "same instant in another zone: already claimed")
	ok, err = b.ClaimRun(ctxA, "s1", fire.Add(time.Minute))
	require.NoError(t, err)
	assert.True(t, ok, "next fire time")
	ok, err = b.ClaimRun(tenant.WithID(context.Background(), "b"), "s1", fire)
	require.NoError(t, err)
	assert.True(t, ok, "schedule ids are per tenant")
}

func TestCoordinatorTenantsActiveOnly(t *testing.T) {
	d := pgtest.New(t, "a", "b")
	_, err := d.Owner.Exec("UPDATE tenants SET status = 'suspended' WHERE id = 'b'")
	require.NoError(t, err)
	got, err := newCoordinator(t, d, "n").Tenants(context.Background())
	require.NoError(t, err)
	assert.ElementsMatch(t, []tenant.ID{"a", "default", "_system"}, got, "the system tenant (platform operators) is active too")
}

func TestCoordinatorClientListenerLease(t *testing.T) {
	d := pgtest.New(t)
	ctx := context.Background()
	require.NoError(t, newCoordinator(t, d, "node-a").AcquireClientListener(ctx))
	err := newCoordinator(t, d, "node-b").AcquireClientListener(ctx)
	assert.ErrorContains(t, err, `node "node-a" accepts the client connections`)
	assert.NoError(t, newCoordinator(t, d, "node-a").AcquireClientListener(ctx), "the same node restarting takes it back")
}

func TestCoordinatorAPINodeLeavesScheduledRuns(t *testing.T) {
	d := pgtest.New(t, "a")
	api := newCoordinator(t, d, "node-api")
	api.LeaveScheduledRuns()
	ctx := tenant.WithID(context.Background(), "a")
	fire := time.Date(2026, 11, 24, 10, 5, 0, 0, time.UTC)
	ok, err := api.ClaimRun(ctx, "s1", fire)
	require.NoError(t, err)
	assert.False(t, ok, "an api node never starts a scheduled run")
	ok, err = newCoordinator(t, d, "node-all").ClaimRun(ctx, "s1", fire)
	require.NoError(t, err)
	assert.True(t, ok, "and claims nothing: the node with the clients gets the run")
}
