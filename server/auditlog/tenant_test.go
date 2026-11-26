package auditlog

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	auditlogmigration "github.com/pohcee/procy-server/db/migration/auditlog"
	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/server/auditlog/config"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/tenant"
)

var tenantTestLog = logger.NewLogger("auditlog", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug)

func ctxFor(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

// migrateTo opens a file DB and migrates it to the given schema version (up or down).
func migrateTo(t *testing.T, path string, version uint) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("sqlite3", path)
	require.NoError(t, err)
	require.NoError(t, sqlite.MigrateTo(db, auditlogmigration.FS, version))
	return db
}

func newTenantProvider(t *testing.T) *SQLiteProvider {
	t.Helper()
	p, err := openSQLiteProvider(t.TempDir(), DataSourceOptions)
	require.NoError(t, err)
	t.Cleanup(func() { p.Close() })
	return p
}

func listUsernames(ctx context.Context, t *testing.T, p Provider) []string {
	t.Helper()
	entries, err := p.List(ctx, &query.ListOptions{})
	require.NoError(t, err)
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Username)
	}
	return names
}

func TestAuditLogRequiresTenant(t *testing.T) {
	p := newTenantProvider(t)
	ctx := context.Background()
	assert.ErrorIs(t, p.Save(ctx, &Entry{Timestamp: time.Now()}), tenant.ErrMissing)
	_, err := p.List(ctx, &query.ListOptions{})
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.Count(ctx, &query.ListOptions{})
	assert.ErrorIs(t, err, tenant.ErrMissing)
}

func TestAuditLogIsolatesTenants(t *testing.T) {
	p := newTenantProvider(t)
	a, b := ctxFor("a"), ctxFor("b")
	require.NoError(t, p.Save(a, &Entry{Timestamp: time.Now(), Username: "alice", ID: "x"}))
	require.NoError(t, p.Save(b, &Entry{Timestamp: time.Now(), Username: "bob", ID: "x"}))
	require.NoError(t, p.Save(b, &Entry{Timestamp: time.Now(), Username: "bob2", ID: "y"}))

	assert.Equal(t, []string{"alice"}, listUsernames(a, t, p))
	assert.ElementsMatch(t, []string{"bob", "bob2"}, listUsernames(b, t, p))

	filtered, err := p.List(b, &query.ListOptions{Filters: []query.FilterOption{{
		Column: []string{"affected_id"}, Operator: query.FilterOperatorTypeEQ, Values: []string{"x"},
	}}})
	require.NoError(t, err)
	require.Len(t, filtered, 1)
	assert.Equal(t, "bob", filtered[0].Username)

	count, err := p.Count(a, &query.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	count, err = p.Count(b, &query.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

func newTenantAuditLog(t *testing.T) (*AuditLog, *SQLiteProvider) {
	t.Helper()
	p := newTenantProvider(t)
	return &AuditLog{logger: tenantTestLog, config: config.Config{Enable: true}, provider: p}, p
}

func TestEntrySavesForTheRequestsTenant(t *testing.T) {
	al, p := newTenantAuditLog(t)
	req := httptest.NewRequest("GET", "/", nil)
	reqCtx, cancel := context.WithCancel(ctxFor("a"))
	req = req.WithContext(reqCtx)

	e := al.Entry(ApplicationClient, ActionCreate).WithHTTPRequest(req)
	e.Username = "alice"
	// entries may be saved after the request is done (e.g. when async work completes)
	cancel()
	e.Save()

	assert.Equal(t, []string{"alice"}, listUsernames(ctxFor("a"), t, p))
	assert.Empty(t, listUsernames(ctxFor("b"), t, p))
}

func TestEntryWithContextSavesForThatTenant(t *testing.T) {
	al, p := newTenantAuditLog(t)
	e := al.Entry(ApplicationClientCommand, ActionExecuteDone).WithContext(ctxFor("b"))
	e.Username = "bob"
	e.Save()

	assert.Equal(t, []string{"bob"}, listUsernames(ctxFor("b"), t, p))
	assert.Empty(t, listUsernames(ctxFor("a"), t, p))
}

func TestEntryWithoutTenantIsNotSaved(t *testing.T) {
	al, p := newTenantAuditLog(t)
	assert.NotPanics(t, func() {
		al.Entry(ApplicationClient, ActionCreate).Save()
		al.Entry(ApplicationClient, ActionCreate).WithContext(context.Background()).Save()
	})
	count, err := p.Count(ctxFor(tenant.Default), &query.ListOptions{})
	require.NoError(t, err)
	assert.Zero(t, count)
}

func TestEntryJSONHasNoTenant(t *testing.T) {
	e := &Entry{TenantID: "a"}
	b, err := json.Marshal(e)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "tenant")
}

func TestUpgradeKeepsAuditlogInDefaultTenant(t *testing.T) {
	dir := t.TempDir()
	old := migrateTo(t, filepath.Join(dir, sqliteFilename), 1)
	_, err := old.Exec(`INSERT INTO auditlog (timestamp, application, action, username, remote_ip, affected_id, client_id, client_hostname, request, response) VALUES (?, 'client', 'create', 'legacy', '', '', '', '', '', '')`, time.Now())
	require.NoError(t, err)
	require.NoError(t, old.Close())

	p, err := openSQLiteProvider(dir, DataSourceOptions)
	require.NoError(t, err)
	defer p.Close()
	assert.Equal(t, []string{"legacy"}, listUsernames(ctxFor(tenant.Default), t, p))
}

func TestDowngradeKeepsOnlyDefaultAuditlog(t *testing.T) {
	dir := t.TempDir()
	p, err := openSQLiteProvider(dir, DataSourceOptions)
	require.NoError(t, err)
	require.NoError(t, p.Save(ctxFor(tenant.Default), &Entry{Timestamp: time.Now(), Username: "default"}))
	require.NoError(t, p.Save(ctxFor("other"), &Entry{Timestamp: time.Now(), Username: "other"}))
	require.NoError(t, p.Close())

	db := migrateTo(t, filepath.Join(dir, sqliteFilename), 1)
	defer db.Close()
	var names []string
	require.NoError(t, db.Select(&names, "SELECT username FROM auditlog"))
	assert.Equal(t, []string{"default"}, names)
}

// An old-schema auditlog.db that gets rotated on startup is migrated first, so the dated file opened later reads
// its entries as the default tenant.
func TestRotatedOldAuditlogIsReadAsDefault(t *testing.T) {
	dir := t.TempDir()
	old := migrateTo(t, filepath.Join(dir, sqliteFilename), 1)
	_, err := old.Exec(`INSERT INTO auditlog (timestamp, application, action, username, remote_ip, affected_id, client_id, client_hostname, request, response) VALUES (?, 'client', 'create', 'legacy', '', '', '', '', '', '')`, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.NoError(t, old.Close())

	rotation, err := NewRotationProvider(ctxFor(tenant.Default), tenantTestLog, time.Minute, dir, DataSourceOptions)
	require.NoError(t, err)
	defer rotation.Close()
	assert.Empty(t, listUsernames(ctxFor(tenant.Default), t, rotation), "rotated on init")
	require.NoError(t, rotation.Save(ctxFor(tenant.Default), &Entry{Timestamp: time.Now(), Username: "new"}))
	assert.Equal(t, []string{"new"}, listUsernames(ctxFor(tenant.Default), t, rotation))

	db, err := sqlite.New(path.Join(dir, time.Now().Format(rotatedFilename)), auditlogmigration.FS, DataSourceOptions)
	require.NoError(t, err)
	rotated := &SQLiteProvider{db: db, converter: query.NewSQLConverter(db.DriverName())}
	defer rotated.Close()
	assert.Equal(t, []string{"legacy"}, listUsernames(ctxFor(tenant.Default), t, rotated))
}

func TestRotationLoopStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(ctxFor(tenant.Default))
	rotation, err := NewRotationProvider(ctx, tenantTestLog, time.Hour, t.TempDir(), DataSourceOptions)
	require.NoError(t, err)
	defer rotation.Close()
	cancel()
	select {
	case <-rotation.loopDone:
	case <-time.After(time.Second):
		t.Fatal("rotation loop did not stop")
	}
}
