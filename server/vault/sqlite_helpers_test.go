package vault

import (
	"github.com/pohcee/procy-server/db/migration/vaults"
	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/share/logger"
)

func openTestSqliteProvider(c Config, l *logger.Logger) (*SqliteProvider, error) {
	db, err := sqlite.New(c.GetVaultDBPath(), vaults.FS, DataSourceOptions)
	if err != nil {
		return nil, err
	}
	return NewSqliteProvider(db, l), nil
}
