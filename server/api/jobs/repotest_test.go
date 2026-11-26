package jobs_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	jobsmigration "github.com/pohcee/procy-server/db/migration/jobs"
	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/server/api/jobs"
	"github.com/pohcee/procy-server/server/repotest"
	"github.com/pohcee/procy-server/share/logger"
)

func TestRepositoryContract(t *testing.T) {
	repotest.Jobs(t, func(t *testing.T) jobs.Provider {
		db, err := sqlite.New(":memory:", jobsmigration.FS, sqlite.DataSourceOptions{})
		require.NoError(t, err)
		p := jobs.NewSqliteProvider(db, logger.NewLogger("jobs", logger.LogOutput{}, logger.LogLevelError))
		t.Cleanup(func() { p.Close() })
		return p
	})
}
