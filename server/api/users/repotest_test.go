package users_test

import (
	"os"
	"testing"

	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/repotest"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

// The admin-defined user database serves only the default tenant.
func TestUserDatabaseContract(t *testing.T) {
	log := logger.NewLogger("users-contract", logger.LogOutput{File: os.Stdout}, logger.LogLevelError)
	repotest.Users(t, func(t *testing.T) users.Provider {
		db, err := sqlx.Connect("sqlite3", ":memory:")
		require.NoError(t, err)
		db.SetMaxOpenConns(1) // one in-memory database
		t.Cleanup(func() { db.Close() })
		for _, q := range []string{
			"CREATE TABLE `users` (username TEXT PRIMARY KEY, password TEXT, password_expired BOOLEAN NOT NULL CHECK (password_expired IN (0, 1)) DEFAULT 0, two_fa_send_to TEXT NOT NULL DEFAULT '', totp_secret TEXT NOT NULL DEFAULT '')",
			"CREATE TABLE `groups` (username TEXT, `group` TEXT)",
			"CREATE TABLE `group_details` (name TEXT, permissions TEXT, tunnels_restricted TEXT, commands_restricted TEXT)",
			"CREATE UNIQUE INDEX `group_details_name` ON `group_details` (`name`)",
		} {
			_, err = db.Exec(q)
			require.NoError(t, err)
		}
		p, err := users.NewUserDatabase(db, "users", "groups", "group_details", true, true, true, log)
		require.NoError(t, err)
		return p
	}, repotest.UsersOptions{OnlyTenant: tenant.Default})
}
