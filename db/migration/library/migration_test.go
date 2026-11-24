package library

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/sqlite"
)

// A manual downgrade (schema version set back to 4) followed by a re-upgrade
// must not fail on the tables migration 005 already created.
func TestMigration005RerunOnExistingTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.db")
	db, err := sqlite.New(path, AssetNames(), Asset, sqlite.DataSourceOptions{})
	require.NoError(t, err)

	_, err = db.Exec("INSERT INTO `library_meta` (`key`, `value`) VALUES ('k', 'v')")
	require.NoError(t, err)
	res, err := db.Exec("UPDATE `schema_migrations` SET `version` = 4, `dirty` = 0")
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	require.NoError(t, db.Close())

	db, err = sqlite.New(path, AssetNames(), Asset, sqlite.DataSourceOptions{})
	require.NoError(t, err)
	defer db.Close()

	var version int
	require.NoError(t, db.Get(&version, "SELECT `version` FROM `schema_migrations`"))
	assert.Equal(t, 6, version)
	var v string
	require.NoError(t, db.Get(&v, "SELECT `value` FROM `library_meta` WHERE `key` = 'k'"))
	assert.Equal(t, "v", v)
}
