package auditlog_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	auditlogmigration "github.com/pohcee/procy-server/db/migration/auditlog"
	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/server/auditlog"
	"github.com/pohcee/procy-server/server/repotest"
)

func TestSQLiteProviderContract(t *testing.T) {
	repotest.AuditLog(t, func(t *testing.T) auditlog.Provider {
		db, err := sqlite.New(":memory:", auditlogmigration.FS, sqlite.DataSourceOptions{})
		require.NoError(t, err)
		p := auditlog.NewSQLiteProvider(db)
		t.Cleanup(func() { _ = p.Close() })
		return p
	})
}
