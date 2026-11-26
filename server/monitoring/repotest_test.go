package monitoring_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	monitoringmigration "github.com/pohcee/procy-server/db/migration/monitoring"
	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/server/monitoring"
	"github.com/pohcee/procy-server/server/repotest"
	"github.com/pohcee/procy-server/share/logger"
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
