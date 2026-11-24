package clientsauth_test

import (
	"testing"

	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/clientsauth"
	"github.com/openrport/openrport/server/repotest"
	"github.com/openrport/openrport/share/tenant"
)

// The admin-defined client auth table serves only the default tenant.
func TestDatabaseProviderContract(t *testing.T) {
	repotest.ClientAuth(t, func(t *testing.T) clientsauth.Provider {
		db, err := sqlx.Connect("sqlite3", ":memory:")
		require.NoError(t, err)
		db.SetMaxOpenConns(1) // one in-memory database
		t.Cleanup(func() { db.Close() })
		_, err = db.Exec("CREATE TABLE clients_auth (id TEXT PRIMARY KEY, password TEXT)")
		require.NoError(t, err)
		return clientsauth.NewDatabaseProvider(db, "clients_auth")
	}, repotest.ClientAuthOptions{OnlyTenant: tenant.Default})
}
