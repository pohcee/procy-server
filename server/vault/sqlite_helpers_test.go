package vault

import (
	"github.com/openrport/openrport/db/migration/vaults"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/share/logger"
)

func openTestSqliteProvider(c Config, l *logger.Logger) (*SqliteProvider, error) {
	db, err := sqlite.New(c.GetVaultDBPath(), vaults.FS, DataSourceOptions)
	if err != nil {
		return nil, err
	}
	return NewSqliteProvider(db, l), nil
}
