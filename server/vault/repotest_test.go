package vault_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/migration/vaults"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/repotest"
	"github.com/openrport/openrport/server/vault"
	"github.com/openrport/openrport/share/logger"
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
