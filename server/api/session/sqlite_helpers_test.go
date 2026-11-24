package session

import (
	"github.com/openrport/openrport/db/migration/api_sessions"
	"github.com/openrport/openrport/db/sqlite"
)

func openTestSqliteProvider(dbPath string, dataSourceOptions sqlite.DataSourceOptions) (*SqliteProvider, error) {
	db, err := sqlite.New(dbPath, api_sessions.FS, dataSourceOptions)
	if err != nil {
		return nil, err
	}
	return NewSqliteProvider(db), nil
}
