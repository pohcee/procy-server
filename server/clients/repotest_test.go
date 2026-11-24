package clients_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	migration "github.com/openrport/openrport/db/migration/clients"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/repotest"
)

func TestSQLiteClientStoreContract(t *testing.T) {
	repotest.ClientStore(t, func(t *testing.T, keep *time.Duration) clients.ClientStore {
		db, err := sqlite.New(":memory:", migration.FS, sqlite.DataSourceOptions{})
		require.NoError(t, err)
		s := clients.NewSQLiteStore(db, keep)
		t.Cleanup(func() { s.Close() })
		return s
	})
}
