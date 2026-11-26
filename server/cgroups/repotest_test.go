package cgroups_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/db/migration/client_groups"
	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/server/cgroups"
	"github.com/pohcee/procy-server/server/repotest"
)

func TestSQLiteClientGroupsContract(t *testing.T) {
	repotest.ClientGroups(t, func(t *testing.T) cgroups.ClientGroupProvider {
		db, err := sqlite.New(":memory:", client_groups.FS, sqlite.DataSourceOptions{})
		require.NoError(t, err)
		p, err := cgroups.NewSqliteProvider(db)
		require.NoError(t, err)
		t.Cleanup(func() { p.Close() })
		return p
	})
}
