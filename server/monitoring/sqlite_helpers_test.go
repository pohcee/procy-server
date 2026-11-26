package monitoring

import (
	"github.com/pohcee/procy-server/db/migration/monitoring"
	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/share/logger"
)

func openTestSqliteProvider(dbPath string, dataSourceOptions sqlite.DataSourceOptions, l *logger.Logger) (DBProvider, error) {
	db, err := sqlite.New(dbPath, monitoring.FS, dataSourceOptions)
	if err != nil {
		return nil, err
	}
	return NewSqliteProvider(db, l), nil
}
