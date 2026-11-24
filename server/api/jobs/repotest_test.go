package jobs_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	jobsmigration "github.com/openrport/openrport/db/migration/jobs"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/repotest"
	"github.com/openrport/openrport/share/logger"
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
