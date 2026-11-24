package cgroups_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/migration/client_groups"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/cgroups"
	"github.com/openrport/openrport/server/repotest"
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
