package auditlog

import (
	"path"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/migration/auditlog"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
)

var dso = sqlite.DataSourceOptions{WALEnabled: true}

func TestRotation(t *testing.T) {
	ctx := ctxFor(tenant.Default)
	dir := t.TempDir()
	period := 300 * time.Millisecond

	// Prepare sqlite with 1 entry
	sqlite, err := openSQLiteProvider(dir, dso)
	require.NoError(t, err)
	err = sqlite.Save(ctx, &Entry{Timestamp: time.Now(), Username: "test1"})
	require.NoError(t, err)
	err = sqlite.Close()
	require.NoError(t, err)

	// No rotation on init if entry is not older than period
	rotation, err := NewRotationProvider(ctx, nil, period, dir, dso)
	require.NoError(t, err)
	entries, err := rotation.List(ctx, &query.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, len(entries))
	assert.Equal(t, "test1", entries[0].Username)
	err = rotation.Close()
	require.NoError(t, err)

	time.Sleep(period)

	// Should rotate on init
	rotation, err = NewRotationProvider(ctx, nil, period, dir, dso)
	require.NoError(t, err)
	entries, err = rotation.List(ctx, &query.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 0, len(entries))
	assertRotatedSqlite(t, dir, "test1")

	// Add entry
	err = rotation.Save(ctx, &Entry{Timestamp: time.Now(), Username: "test2"})
	require.NoError(t, err)

	// Read back entry
	entries, err = rotation.List(ctx, &query.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, len(entries))
	assert.Equal(t, "test2", entries[0].Username)

	time.Sleep(period)

	// Should be rotated after period
	entries, err = rotation.List(ctx, &query.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 0, len(entries))
	err = rotation.Close()
	require.NoError(t, err)
	assertRotatedSqlite(t, dir, "test2")
}

func assertRotatedSqlite(t *testing.T, dir, expectedUsername string) {
	db, err := sqlite.New(path.Join(dir, time.Now().Format(rotatedFilename)), auditlog.FS, dso)
	require.NoError(t, err)
	sqlite := &SQLiteProvider{
		db: db,
	}
	defer sqlite.Close()

	entries, err := sqlite.List(ctxFor(tenant.Default), &query.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, len(entries))
	assert.Equal(t, expectedUsername, entries[0].Username)
}

// TestRotationCreatesMigratedFile checks that the file rotation creates gets the schema, whatever opened the first one.
func TestRotationCreatesMigratedFile(t *testing.T) {
	ctx := ctxFor(tenant.Default)
	dir := t.TempDir()

	rotation, err := NewRotationProvider(ctx, nil, time.Hour, dir, dso)
	require.NoError(t, err)
	defer rotation.Close()

	require.NoError(t, rotation.rotate())

	require.NoError(t, rotation.Save(ctx, &Entry{Timestamp: time.Now(), Username: "after-rotation"}))
	entries, err := rotation.List(ctx, &query.ListOptions{})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "after-rotation", entries[0].Username)
}
