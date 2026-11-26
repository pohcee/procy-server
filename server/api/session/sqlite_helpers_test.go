package session

import (
	"github.com/pohcee/procy-server/db/migration/api_sessions"
	"github.com/pohcee/procy-server/db/sqlite"
)

func openTestSqliteProvider(dbPath string, dataSourceOptions sqlite.DataSourceOptions) (*SqliteProvider, error) {
	db, err := sqlite.New(dbPath, api_sessions.FS, dataSourceOptions)
	if err != nil {
		return nil, err
	}
	return NewSqliteProvider(db), nil
}
