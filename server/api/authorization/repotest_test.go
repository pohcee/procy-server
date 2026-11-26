package authorization_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/db/migration/api_token"
	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/server/api/authorization"
	"github.com/pohcee/procy-server/server/repotest"
)

func TestSqliteProviderContract(t *testing.T) {
	repotest.APITokens(t, func(t *testing.T) authorization.DbProvider {
		db, err := sqlite.New(":memory:", api_token.FS, sqlite.DataSourceOptions{})
		require.NoError(t, err)
		p := authorization.NewSqliteProvider(db)
		t.Cleanup(func() { p.Close() })
		return p
	})
}
