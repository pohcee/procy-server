package session

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	bindata "github.com/golang-migrate/migrate/v4/source/go_bindata"
	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/migration/api_sessions"
	"github.com/openrport/openrport/share/tenant"
)

func ctxFor(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

func liveSession(username string) APISession {
	now := time.Now().UTC()
	return APISession{Username: username, ExpiresAt: now.Add(time.Hour), LastAccessAt: now, UserAgent: "ua", IPAddress: "1.2.3.4"}
}

func expiredSession(username string) APISession {
	now := time.Now().UTC()
	return APISession{Username: username, ExpiresAt: now.Add(-time.Minute), LastAccessAt: now, UserAgent: "ua", IPAddress: "1.2.3.4"}
}

// migrateSessionsTo opens a file DB and migrates it to the given schema version (up or down).
func migrateSessionsTo(t *testing.T, path string, version uint) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("sqlite3", path)
	require.NoError(t, err)
	src, err := bindata.WithInstance(bindata.Resource(api_sessions.AssetNames(), api_sessions.Asset))
	require.NoError(t, err)
	drv, err := sqlite3.WithInstance(db.DB, &sqlite3.Config{})
	require.NoError(t, err)
	m, err := migrate.NewWithInstance("go-bindata", src, "sqlite3", drv)
	require.NoError(t, err)
	err = m.Migrate(version)
	if err != migrate.ErrNoChange {
		require.NoError(t, err)
	}
	return db
}

func TestSessionStorageRequiresTenant(t *testing.T) {
	p, err := NewSqliteProvider(":memory:", DataSourceOptions)
	require.NoError(t, err)
	defer p.Close()
	ctx := context.Background()

	_, _, err = p.Get(ctx, 1)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.GetAll(ctx)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.Save(ctx, liveSession("u"))
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.Save(ctx, APISession{SessionID: 1, Username: "u"})
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.ErrorIs(t, p.Delete(ctx, 1), tenant.ErrMissing)
	assert.ErrorIs(t, p.DeleteExpired(ctx), tenant.ErrMissing)
	assert.ErrorIs(t, p.DeleteAllByUser(ctx, "u"), tenant.ErrMissing)
	assert.ErrorIs(t, p.DeleteByID(ctx, "u", 1), tenant.ErrMissing)
}

func TestSessionStorageIsolatesTenants(t *testing.T) {
	p, err := NewSqliteProvider(":memory:", DataSourceOptions)
	require.NoError(t, err)
	defer p.Close()
	a, b := ctxFor("a"), ctxFor("b")

	aID, err := p.Save(a, liveSession("u"))
	require.NoError(t, err)
	bID, err := p.Save(b, liveSession("u"))
	require.NoError(t, err)
	aExpired, err := p.Save(a, expiredSession("u"))
	require.NoError(t, err)
	bExpired, err := p.Save(b, expiredSession("u"))
	require.NoError(t, err)

	found, _, err := p.Get(b, aID)
	require.NoError(t, err)
	assert.False(t, found)
	found, got, err := p.Get(a, aID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, tenant.ID("a"), got.TenantID)

	all, err := p.GetAll(a)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, aID, all[0].SessionID)

	// b can't update a's session
	_, err = p.Save(b, APISession{SessionID: aID, Username: "u", ExpiresAt: time.Now().Add(time.Hour), UserAgent: "hijack"})
	require.NoError(t, err)
	_, got, err = p.Get(a, aID)
	require.NoError(t, err)
	assert.Equal(t, "ua", got.UserAgent)

	// deletes in b don't touch a
	require.NoError(t, p.Delete(b, aID))
	require.NoError(t, p.DeleteByID(b, "u", aID))
	require.NoError(t, p.DeleteExpired(b))
	found, _, err = p.Get(a, aExpired)
	require.NoError(t, err)
	assert.True(t, found, "cleanup in b must not delete a's expired sessions")
	found, _, err = p.Get(b, bExpired)
	require.NoError(t, err)
	assert.False(t, found)

	require.NoError(t, p.DeleteAllByUser(b, "u"))
	found, _, err = p.Get(a, aID)
	require.NoError(t, err)
	assert.True(t, found)
	found, _, err = p.Get(b, bID)
	require.NoError(t, err)
	assert.False(t, found)
}

func TestSessionCacheRequiresTenant(t *testing.T) {
	storage, err := NewSqliteProvider(":memory:", DataSourceOptions)
	require.NoError(t, err)
	defer storage.Close()

	_, err = NewCache(context.Background(), time.Hour, time.Hour, storage, nil)
	assert.ErrorIs(t, err, tenant.ErrMissing)

	c, err := NewCache(ctxFor("a"), time.Hour, time.Hour, storage, nil)
	require.NoError(t, err)
	ctx := context.Background()
	_, _, err = c.Get(ctx, 1)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = c.GetAllByUser(ctx, "u")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = c.Save(ctx, liveSession("u"))
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.ErrorIs(t, c.Delete(ctx, 1), tenant.ErrMissing)
	assert.ErrorIs(t, c.DeleteByID(ctx, "u", 1), tenant.ErrMissing)
	assert.ErrorIs(t, c.DeleteAllByUser(ctx, "u"), tenant.ErrMissing)
	assert.ErrorIs(t, c.DeleteExpired(ctx), tenant.ErrMissing)
}

func TestSessionCacheIsolatesTenants(t *testing.T) {
	storage, err := NewSqliteProvider(":memory:", DataSourceOptions)
	require.NoError(t, err)
	defer storage.Close()
	a, b := ctxFor("a"), ctxFor("b")

	// a session saved before the cache starts is loaded for the cache's tenant only
	preA, err := storage.Save(a, liveSession("u"))
	require.NoError(t, err)
	c, err := NewCache(a, time.Hour, time.Hour, storage, nil)
	require.NoError(t, err)
	found, _, err := c.Get(a, preA)
	require.NoError(t, err)
	assert.True(t, found)
	found, _, err = c.Get(b, preA)
	require.NoError(t, err)
	assert.False(t, found)

	aID, err := c.Save(a, liveSession("u"))
	require.NoError(t, err)
	bID, err := c.Save(b, liveSession("u"))
	require.NoError(t, err)

	found, got, err := c.Get(b, bID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, tenant.ID("b"), got.TenantID)
	found, _, err = c.Get(b, aID)
	require.NoError(t, err)
	assert.False(t, found)

	sessA, err := c.GetAllByUser(a, "u")
	require.NoError(t, err)
	assert.Len(t, sessA, 2)
	sessB, err := c.GetAllByUser(b, "u")
	require.NoError(t, err)
	require.Len(t, sessB, 1)
	assert.Equal(t, bID, sessB[0].SessionID)

	require.NoError(t, c.Delete(b, aID))
	require.NoError(t, c.DeleteByID(b, "u", aID))
	found, _, err = c.Get(a, aID)
	require.NoError(t, err)
	assert.True(t, found)

	require.NoError(t, c.DeleteAllByUser(b, "u"))
	found, _, err = c.Get(a, aID)
	require.NoError(t, err)
	assert.True(t, found)
	found, _, err = storage.Get(a, aID)
	require.NoError(t, err)
	assert.True(t, found)
	found, _, err = c.Get(b, bID)
	require.NoError(t, err)
	assert.False(t, found)
}

func TestSessionCleanupTaskScopesByTenant(t *testing.T) {
	storage, err := NewSqliteProvider(":memory:", DataSourceOptions)
	require.NoError(t, err)
	defer storage.Close()
	aExpired, err := storage.Save(ctxFor("a"), expiredSession("u"))
	require.NoError(t, err)
	bExpired, err := storage.Save(ctxFor("b"), expiredSession("u"))
	require.NoError(t, err)
	c, err := NewCache(ctxFor("a"), time.Hour, time.Hour, storage, nil)
	require.NoError(t, err)

	assert.ErrorIs(t, NewCleanupTask(c).Run(context.Background()), tenant.ErrMissing)
	require.NoError(t, NewCleanupTask(c).Run(ctxFor("a")))

	found, _, err := storage.Get(ctxFor("a"), aExpired)
	require.NoError(t, err)
	assert.False(t, found)
	found, _, err = storage.Get(ctxFor("b"), bExpired)
	require.NoError(t, err)
	assert.True(t, found)
}

func TestSessionTenantIDNotInJSON(t *testing.T) {
	s := liveSession("u")
	s.TenantID = "a"
	b, err := json.Marshal(s)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "tenant")
}

func TestSessionUpgradeKeepsSessionsInDefaultTenant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_sessions.db")
	old := migrateSessionsTo(t, path, 2)
	_, err := old.Exec(`INSERT INTO api_sessions (session_id, expires_at, username, last_access_at, user_agent, ip_address) VALUES (7, ?, 'legacy', ?, 'ua', '1.2.3.4')`,
		time.Now().Add(time.Hour), time.Now())
	require.NoError(t, err)
	require.NoError(t, old.Close())

	p, err := NewSqliteProvider(path, DataSourceOptions)
	require.NoError(t, err)
	defer p.Close()
	found, got, err := p.Get(ctxFor(tenant.Default), 7)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "legacy", got.Username)
	assert.Equal(t, tenant.Default, got.TenantID)

	// new sessions don't reuse the existing id
	id, err := p.Save(ctxFor(tenant.Default), liveSession("new"))
	require.NoError(t, err)
	assert.Greater(t, id, int64(7))
}

func TestSessionDowngradeKeepsOnlyDefaultTenant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_sessions.db")
	p, err := NewSqliteProvider(path, DataSourceOptions)
	require.NoError(t, err)
	_, err = p.Save(ctxFor(tenant.Default), liveSession("default-user"))
	require.NoError(t, err)
	_, err = p.Save(ctxFor("a"), liveSession("a-user"))
	require.NoError(t, err)
	var before int
	require.NoError(t, p.db.Get(&before, `SELECT COUNT(*) FROM api_sessions`))
	require.Equal(t, 2, before)
	require.NoError(t, p.Close())

	down := migrateSessionsTo(t, path, 2)
	defer down.Close()
	var users []string
	require.NoError(t, down.Select(&users, `SELECT username FROM api_sessions`))
	assert.Equal(t, []string{"default-user"}, users)

	var n int
	require.NoError(t, down.Get(&n, `SELECT COUNT(*) FROM pragma_table_info('api_sessions') WHERE name = 'tenant_id'`))
	assert.Zero(t, n)
	require.NoError(t, down.Get(&n, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name IN ('idx_username', 'idx_expires_at_time')`))
	assert.Equal(t, 2, n)
}
