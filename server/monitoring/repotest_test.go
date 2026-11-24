package monitoring_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	monitoringmigration "github.com/openrport/openrport/db/migration/monitoring"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/monitoring"
	"github.com/openrport/openrport/server/repotest"
	"github.com/openrport/openrport/share/logger"
)

func TestSqliteProviderContract(t *testing.T) {
	l := logger.NewLogger("monitoring", logger.LogOutput{File: os.Stdout}, logger.LogLevelError)
	repotest.Monitoring(t, func(t *testing.T) monitoring.DBProvider {
		db, err := sqlite.New(":memory:", monitoringmigration.FS, sqlite.DataSourceOptions{})
		require.NoError(t, err)
		p := monitoring.NewSqliteProvider(db, l)
		t.Cleanup(func() { _ = p.Close() })
		return p
	})
}
