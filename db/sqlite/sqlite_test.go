package sqlite

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	sql "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/db/migration/dummy"
	"github.com/pohcee/procy-server/share/logger"
)

func TestSqliteWALEnabled(t *testing.T) {
	dataSourceName := t.TempDir() + "/test-db.sqlite3"
	_, err := New(dataSourceName, dummy.FS, DataSourceOptions{WALEnabled: true})
	require.NoError(t, err)
	_, err = os.Stat(dataSourceName + "-shm")
	require.NoError(t, err)
	_, err = os.Stat(dataSourceName + "-wal")
	require.NoError(t, err)
}

func TestSqliteWALDisabled(t *testing.T) {
	dataSourceName := t.TempDir() + "/test-db.sqlite3"
	_, err := New(dataSourceName, dummy.FS, DataSourceOptions{WALEnabled: false})
	require.NoError(t, err)
	_, err = os.Stat(dataSourceName + "-shm")
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(dataSourceName + "-wal")
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestCheckVersion(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"), DataSourceOptions{})
	require.NoError(t, err)
	defer db.Close()

	err = CheckVersion(db, dummy.FS)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `run "rportd migrate"`)

	require.NoError(t, Migrate(db, dummy.FS))
	assert.NoError(t, CheckVersion(db, dummy.FS))

	_, err = db.Exec("UPDATE schema_migrations SET dirty = 1")
	require.NoError(t, err)
	err = CheckVersion(db, dummy.FS)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `run "rportd migrate"`)
	assert.Contains(t, err.Error(), "schema_migrations")
}

func TestCheckVersionRefusesNewerDB(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"), DataSourceOptions{})
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, Migrate(db, dummy.FS))
	latest, err := LatestVersion(dummy.FS)
	require.NoError(t, err)

	_, err = db.Exec("UPDATE schema_migrations SET version = ?", latest+1)
	require.NoError(t, err)

	err = CheckVersion(db, dummy.FS)
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("database is at version %d, newer than this rportd (%d)", latest+1, latest))
}

func TestOpenDoesNotMigrate(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"), DataSourceOptions{})
	require.NoError(t, err)
	defer db.Close()

	var tables int
	require.NoError(t, db.Get(&tables, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table'"))
	assert.Equal(t, 0, tables)

	require.Error(t, CheckVersion(db, dummy.FS))
	require.NoError(t, db.Get(&tables, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table'"))
	assert.Equal(t, 0, tables, "CheckVersion must not change the db")
}

func TestNewMigrates(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "test.db"), dummy.FS, DataSourceOptions{})
	require.NoError(t, err)
	defer db.Close()
	assert.NoError(t, CheckVersion(db, dummy.FS))
	var n int
	assert.NoError(t, db.Get(&n, "SELECT COUNT(*) FROM dummy"))
}

func TestNewReturnsErrorWhenMigrateFails(t *testing.T) {
	bad := fstest.MapFS{"001_bad.up.sql": {Data: []byte("NOT SQL;")}}
	db, err := New(filepath.Join(t.TempDir(), "test.db"), bad, DataSourceOptions{})
	assert.Error(t, err)
	assert.Nil(t, db)
}

func TestShouldSucceedWhenNoError(t *testing.T) {
	testLog := logger.NewLogger("retries", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug)

	_, err := WithRetryWhenBusy(func() (result any, err error) {
		return nil, nil
	}, "test", testLog)

	assert.NoError(t, err)
}

func TestShouldSucceedAfterRetries(t *testing.T) {
	testLog := logger.NewLogger("retries", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug)

	attempts := 0

	_, err := WithRetryWhenBusy(func() (result any, err error) {
		if attempts < 2 {
			attempts++
			return nil, sql.Error{
				Code: sql.ErrBusy,
			}
		}
		return nil, nil
	}, "test", testLog)

	assert.NoError(t, err)
	assert.Equal(t, 2, attempts)
}

func TestShouldFailWhenMaxBusyErrors(t *testing.T) {
	testLog := logger.NewLogger("retries", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug)

	attempts := 0

	_, err := WithRetryWhenBusy(func() (result any, err error) {
		attempts++
		return nil, sql.Error{
			Code: sql.ErrBusy,
		}
	}, "test", testLog)

	assert.EqualError(t, err, sql.ErrBusy.Error())
	assert.Equal(t, DefaultMaxAttempts, attempts)
}

func TestShouldFailImmediatelyWhenNonBusyError(t *testing.T) {
	testLog := logger.NewLogger("retries", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug)

	attempts := 0

	_, err := WithRetryWhenBusy(func() (result any, err error) {
		attempts++
		// fail immediately when not sql.ErrBusy
		return nil, sql.Error{
			Code: sql.ErrCorrupt,
		}
	}, "test", testLog)

	assert.EqualError(t, err, sql.ErrCorrupt.Error())
	assert.Equal(t, 1, attempts)
}
