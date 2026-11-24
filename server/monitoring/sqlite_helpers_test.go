package monitoring

import (
	"github.com/openrport/openrport/db/migration/monitoring"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/share/logger"
)

func openTestSqliteProvider(dbPath string, dataSourceOptions sqlite.DataSourceOptions, l *logger.Logger) (DBProvider, error) {
	db, err := sqlite.New(dbPath, monitoring.FS, dataSourceOptions)
	if err != nil {
		return nil, err
	}
	return NewSqliteProvider(db, l), nil
}
