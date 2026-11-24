package sqlite_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/notifications"
	repo "github.com/openrport/openrport/server/notifications/repository/sqlite"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
)

func ctxFor(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

// migrateTo opens a file DB and migrates it to the given schema version (up or down).
func migrateTo(t *testing.T, path string, version uint) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("sqlite3", path)
	require.NoError(t, err)
	require.NoError(t, sqlite.MigrateTo(db, repo.Migrations, version))
	return db
}

func newTenantRepo(t *testing.T) repo.Repository {
	t.Helper()
	db, err := sqlite.New(":memory:", repo.Migrations, sqlite.DataSourceOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return repo.NewRepository(db, testLog)
}

func TestNotificationsRequireTenant(t *testing.T) {
	r := newTenantRepo(t)
	ctx := context.Background()
	n := GenerateNotification()

	_, err := r.List(ctx, &query.ListOptions{})
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = r.Count(ctx, &query.ListOptions{})
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, _, err = r.Details(ctx, n.ID.ID())
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.ErrorIs(t, r.Create(ctx, n), tenant.ErrMissing)
	assert.ErrorIs(t, r.SetDone(ctx, n, "out"), tenant.ErrMissing)
	assert.ErrorIs(t, r.SetError(ctx, n, "out", "err"), tenant.ErrMissing)
}

func TestNotificationsIsolateTenants(t *testing.T) {
	r := newTenantRepo(t)
	a, b := ctxFor("a"), ctxFor("b")
	n := GenerateNotification()

	// the same notification ID can exist in both tenants
	require.NoError(t, r.SetDone(a, n, "from a"))
	require.NoError(t, r.SetError(b, n, "from b", "boom"))
	require.NoError(t, r.SetDone(b, GenerateNotification(), ""))

	got, found, err := r.Details(a, n.ID.ID())
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "from a", got.Out)
	assert.Equal(t, notifications.ProcessingStateDone, got.State)
	assert.Equal(t, tenant.ID("a"), got.Tenant)

	got, found, err = r.Details(b, n.ID.ID())
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "from b", got.Out)
	assert.Equal(t, notifications.ProcessingStateError, got.State)

	listA, err := r.List(a, &query.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, listA, 1)
	listB, err := r.List(b, &query.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, listB, 2)

	countA, err := r.Count(a, &query.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, countA)
	countB, err := r.Count(b, &query.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, countB)

	_, found, err = r.Details(ctxFor("c"), n.ID.ID())
	require.NoError(t, err)
	assert.False(t, found)
}

func TestCreateStampsTheContextsTenant(t *testing.T) {
	r := newTenantRepo(t)
	n := GenerateNotification()
	n.Tenant = "b"
	require.NoError(t, r.Create(ctxFor("a"), n))

	queued := <-r.NotificationStream(notifications.TargetScript)
	assert.Equal(t, tenant.ID("a"), queued.Tenant)
}

func TestNotificationDetailsJSONHasNoTenant(t *testing.T) {
	n := GenerateNotification()
	n.Tenant = "a"
	b, err := json.Marshal(n)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "enant")
}

func TestUpgradeKeepsNotificationsInDefaultTenant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.db")
	old := migrateTo(t, path, 1)
	_, err := old.Exec("INSERT INTO notifications_log (notification_id, reference_id, transport, state, out) VALUES ('01HXLEGACY0000000000000000', 'Problem:::legacy', 'smtp', 'done', 'old')")
	require.NoError(t, err)
	require.NoError(t, old.Close())

	db, err := sqlite.New(path, repo.Migrations, sqlite.DataSourceOptions{})
	require.NoError(t, err)
	defer db.Close()
	r := repo.NewRepository(db, testLog)

	got, found, err := r.Details(ctxFor(tenant.Default), "01HXLEGACY0000000000000000")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "old", got.Out)
}

func TestDowngradeKeepsOnlyDefaultNotifications(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.db")
	db, err := sqlite.New(path, repo.Migrations, sqlite.DataSourceOptions{})
	require.NoError(t, err)
	r := repo.NewRepository(db, testLog)
	n := GenerateNotification()
	require.NoError(t, r.SetDone(ctxFor(tenant.Default), n, "default"))
	require.NoError(t, r.SetDone(ctxFor("other"), n, "other"))
	require.NoError(t, db.Close())

	old := migrateTo(t, path, 1)
	defer old.Close()
	var outs []string
	require.NoError(t, old.Select(&outs, "SELECT out FROM notifications_log"))
	assert.Equal(t, []string{"default"}, outs)
}

func TestCleanerOnlyCleansItsTenant(t *testing.T) {
	db, err := sqlite.New(":memory:", repo.Migrations, sqlite.DataSourceOptions{})
	require.NoError(t, err)
	defer db.Close()
	r := repo.NewRepository(db, testLog)
	require.NoError(t, r.SetDone(ctxFor("a"), GenerateNotification(), ""))
	require.NoError(t, r.SetDone(ctxFor("b"), GenerateNotification(), ""))

	// keepFor < 0 makes everything old enough
	c := repo.StartCleaner(ctxFor("a"), logger.NewLogger("cleaner", logger.NewLogOutput(""), logger.LogLevelInfo), r, -time.Hour, time.Hour)
	defer c.Close()

	assert.Eventually(t, func() bool {
		n, err := r.Count(ctxFor("a"), &query.ListOptions{})
		return err == nil && n == 0
	}, time.Second, 10*time.Millisecond)
	n, err := r.Count(ctxFor("b"), &query.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}
