package postgres_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/migrate"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
)

func TestCheckAppRole(t *testing.T) {
	d := pgtest.New(t)
	assert.NoError(t, postgres.CheckAppRole(context.Background(), d.App))
	assert.ErrorContains(t, postgres.CheckAppRole(context.Background(), d.Owner), "row-level security")
}

// With a schema owner that is not a superuser (the documented setup), CheckAppRole refuses the owner (it owns the
// tables) and a role that inherits the owner's rights, and accepts a separate role granted with migrate.GrantApp.
func TestCheckAppRoleNonSuperuserOwner(t *testing.T) {
	ctx := context.Background()
	d := pgtest.NewEmpty(t)
	suffix := strings.TrimPrefix(d.Name, "t_")
	owner, member, app := "own_"+suffix, "mem_"+suffix, "app_"+suffix
	for _, stmt := range []string{
		"CREATE ROLE " + owner + " LOGIN PASSWORD 'pw' NOSUPERUSER NOBYPASSRLS",
		"CREATE ROLE " + app + " LOGIN PASSWORD 'pw' NOSUPERUSER NOBYPASSRLS",
		"CREATE ROLE " + member + " LOGIN PASSWORD 'pw' NOSUPERUSER NOBYPASSRLS INHERIT",
		"GRANT " + owner + " TO " + member,
		"ALTER DATABASE " + d.Name + " OWNER TO " + owner,
		"ALTER SCHEMA public OWNER TO " + owner,
	} {
		_, err := d.Owner.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	t.Cleanup(func() { // roles are global: hand everything back to the superuser and drop them
		bg := context.Background()
		_, _ = d.Owner.ExecContext(bg, "ALTER DATABASE "+d.Name+" OWNER TO CURRENT_USER")
		_, _ = d.Owner.ExecContext(bg, "REASSIGN OWNED BY "+owner+" TO CURRENT_USER")
		_, _ = d.Owner.ExecContext(bg, "DROP OWNED BY "+owner+", "+app+", "+member+" CASCADE")
		for _, r := range []string{member, app, owner} {
			_, _ = d.Owner.ExecContext(bg, "DROP ROLE IF EXISTS "+r)
		}
	})

	ownerDB := openAs(t, d.OwnerURL, owner)
	_, err := migrate.Up(ctx, ownerDB)
	require.NoError(t, err)
	require.NoError(t, migrate.GrantApp(ctx, ownerDB, app))

	assert.ErrorContains(t, postgres.CheckAppRole(ctx, ownerDB), "owns")
	assert.ErrorContains(t, postgres.CheckAppRole(ctx, openAs(t, d.OwnerURL, member)), "owns")
	assert.NoError(t, postgres.CheckAppRole(ctx, openAs(t, d.OwnerURL, app)))
}

func openAs(t *testing.T, rawURL, role string) *sqlx.DB {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	u.User = url.UserPassword(role, "pw")
	db, err := postgres.Open(context.Background(), u.String())
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}
