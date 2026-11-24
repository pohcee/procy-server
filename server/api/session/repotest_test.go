package session_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/migration/api_sessions"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/api/session"
	"github.com/openrport/openrport/server/repotest"
)

func TestSqliteProviderContract(t *testing.T) {
	repotest.APISessions(t, func(t *testing.T) session.StorageProvider {
		db, err := sqlite.New(":memory:", api_sessions.FS, sqlite.DataSourceOptions{})
		require.NoError(t, err)
		p := session.NewSqliteProvider(db)
		t.Cleanup(func() { p.Close() })
		return p
	})
}
