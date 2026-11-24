package auditlog_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	auditlogmigration "github.com/openrport/openrport/db/migration/auditlog"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/auditlog"
	"github.com/openrport/openrport/server/repotest"
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
