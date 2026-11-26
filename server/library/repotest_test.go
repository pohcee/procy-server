package library_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	librarymigration "github.com/pohcee/procy-server/db/migration/library"
	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/server/library"
	"github.com/pohcee/procy-server/server/repotest"
)

func TestSQLiteStoreContract(t *testing.T) {
	repotest.Library(t, func(t *testing.T) library.Repository {
		db, err := sqlite.New(filepath.Join(t.TempDir(), "library.db"), librarymigration.FS, sqlite.DataSourceOptions{})
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		return library.NewSQLiteStore(db)
	})
}
