package schedule_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	jobsmigration "github.com/openrport/openrport/db/migration/jobs"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/api/jobs/schedule"
	"github.com/openrport/openrport/server/repotest"
	"github.com/openrport/openrport/share/logger"
)

func TestRepositoryContract(t *testing.T) {
	repotest.Schedules(t, func(t *testing.T) repotest.SchedulesRepos {
		db, err := sqlite.New(":memory:", jobsmigration.FS, sqlite.DataSourceOptions{})
		require.NoError(t, err)
		t.Cleanup(func() { db.Close() })
		return repotest.SchedulesRepos{
			Jobs:      jobs.NewSqliteProvider(db, logger.NewLogger("jobs", logger.LogOutput{}, logger.LogLevelError)),
			Schedules: schedule.NewSQLiteProvider(db),
		}
	})
}
