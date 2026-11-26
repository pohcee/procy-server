package storedtunnels_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	migration "github.com/pohcee/procy-server/db/migration/clients"
	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/server/clients"
	"github.com/pohcee/procy-server/server/clients/storedtunnels"
	"github.com/pohcee/procy-server/server/repotest"
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
