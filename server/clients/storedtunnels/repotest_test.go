package storedtunnels_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	migration "github.com/openrport/openrport/db/migration/clients"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clients/storedtunnels"
	"github.com/openrport/openrport/server/repotest"
)

func TestSQLiteStoredTunnelsContract(t *testing.T) {
	repotest.StoredTunnels(t, func(t *testing.T) repotest.StoredTunnelRepos {
		db, err := sqlite.New(":memory:", migration.FS, sqlite.DataSourceOptions{})
		require.NoError(t, err)
		t.Cleanup(func() { db.Close() })
		return repotest.StoredTunnelRepos{
			Clients: clients.NewSQLiteStore(db, nil),
			Tunnels: storedtunnels.NewSQLiteProvider(db),
		}
	})
}
