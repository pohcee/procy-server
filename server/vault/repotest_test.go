package vault_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/db/migration/vaults"
	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/server/repotest"
	"github.com/pohcee/procy-server/server/vault"
	"github.com/pohcee/procy-server/share/logger"
)

func TestSqliteProviderContract(t *testing.T) {
	l := logger.NewLogger("vault", logger.LogOutput{File: os.Stdout}, logger.LogLevelError)
	repotest.Vault(t, func(t *testing.T) vault.DbProvider {
		db, err := sqlite.New(":memory:", vaults.FS, vault.DataSourceOptions)
		require.NoError(t, err)
		p := vault.NewSqliteProvider(db, l)
		t.Cleanup(func() { _ = p.Close() })
		return p
	})
}
