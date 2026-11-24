package postgres_test

import (
	"context"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/migrate"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	"github.com/openrport/openrport/share/tenant"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func ctxFor(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

func TestInTenantRequiresTenant(t *testing.T) {
	called := false
	err := postgres.InTenant(context.Background(), nil, func(*sqlx.Tx) error { called = true; return nil })
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.False(t, called)
}

func TestInTenantSetsSetting(t *testing.T) {
	d := pgtest.New(t, "a")
	var got string
	require.NoError(t, postgres.InTenant(ctxFor("a"), d.App, func(tx *sqlx.Tx) error {
		return tx.Get(&got, "SELECT current_setting('rport.tenant_id')")
	}))
	assert.Equal(t, "a", got)
	// the setting is local to the transaction
	var after *string
	require.NoError(t, d.App.Get(&after, "SELECT current_setting('rport.tenant_id', true)"))
	if after != nil {
		assert.Equal(t, "", *after)
	}
}

// scratch creates a tenant-scoped table with row-level security, as domain migrations do.
func scratch(t *testing.T, d *pgtest.DB) {
	t.Helper()
	for _, s := range []string{
		"CREATE TABLE scratch (tenant_id text NOT NULL REFERENCES tenants(id), id text NOT NULL, PRIMARY KEY (tenant_id, id))",
		"SELECT rport_enable_rls('scratch')",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON scratch TO " + pgtest.AppRole,
	} {
		_, err := d.Owner.Exec(s)
		require.NoError(t, err)
	}
}

func TestRLSHidesOtherTenants(t *testing.T) {
	d := pgtest.New(t, "a", "b")
	scratch(t, d)
	for _, id := range []tenant.ID{"a", "b"} {
		id := id
		require.NoError(t, postgres.InTenant(ctxFor(id), d.App, func(tx *sqlx.Tx) error {
			_, err := tx.Exec("INSERT INTO scratch (tenant_id, id) VALUES ($1, 'x')", string(id))
			return err
		}))
	}
	// no WHERE clause: row-level security alone keeps b's row out
	var tenants []string
	require.NoError(t, postgres.InTenant(ctxFor("a"), d.App, func(tx *sqlx.Tx) error {
		return tx.Select(&tenants, "SELECT tenant_id FROM scratch")
	}))
	assert.Equal(t, []string{"a"}, tenants)

	// writing another tenant's row is refused
	err := postgres.InTenant(ctxFor("a"), d.App, func(tx *sqlx.Tx) error {
		_, err := tx.Exec("INSERT INTO scratch (tenant_id, id) VALUES ('b', 'y')")
		return err
	})
	assert.ErrorContains(t, err, "row-level security")

	// updates and deletes can't reach it either
	require.NoError(t, postgres.InTenant(ctxFor("a"), d.App, func(tx *sqlx.Tx) error {
		res, err := tx.Exec("DELETE FROM scratch WHERE tenant_id = 'b'")
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		assert.Zero(t, n)
		return nil
	}))
}

func TestRLSWithoutTenantSeesNothing(t *testing.T) {
	d := pgtest.New(t, "a")
	scratch(t, d)
	_, err := d.Owner.Exec("SET rport.tenant_id = 'a'; INSERT INTO scratch (tenant_id, id) VALUES ('a', 'x')")
	require.NoError(t, err)
	var n int
	require.NoError(t, d.App.Get(&n, "SELECT count(*) FROM scratch"))
	assert.Zero(t, n)
	require.NoError(t, d.Owner.Get(&n, "SELECT count(*) FROM scratch"))
	assert.Equal(t, 1, n, "the owner (superuser) bypasses row-level security; setup only")
}

func TestMigrateIdempotent(t *testing.T) {
	d := pgtest.New(t)
	applied, err := migrate.Up(context.Background(), d.Owner)
	require.NoError(t, err)
	assert.Empty(t, applied)
	require.NoError(t, migrate.Check(context.Background(), d.Owner))
}

func TestMigrateConcurrent(t *testing.T) {
	d := pgtest.New(t)
	// a fresh database in the same cluster, migrated by two runs at once
	_, err := d.Owner.Exec("DROP TABLE schema_migrations")
	require.NoError(t, err)
	_, err = d.Owner.Exec("DROP SCHEMA public CASCADE; CREATE SCHEMA public")
	require.NoError(t, err)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := migrate.Up(context.Background(), d.Owner); errs <- err }()
	}
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	require.NoError(t, migrate.Check(context.Background(), d.Owner))
}

func TestCheckFailsOnEmptyDatabase(t *testing.T) {
	d := pgtest.New(t)
	_, err := d.Owner.Exec("DROP SCHEMA public CASCADE; CREATE SCHEMA public")
	require.NoError(t, err)
	assert.ErrorContains(t, migrate.Check(context.Background(), d.Owner), "rportd migrate")
}

func TestTenantIDFormat(t *testing.T) {
	d := pgtest.New(t)
	_, err := d.Owner.Exec("INSERT INTO tenants (id) VALUES ('Bad Tenant')")
	assert.Error(t, err)
}

func TestGrantAppLeastPrivilege(t *testing.T) {
	d := pgtest.New(t, "a")
	for _, q := range []string{
		"INSERT INTO tenants (id) VALUES ('x')",
		"INSERT INTO user_logins (username, tenant_id) VALUES ('u', 'a')",
		"DELETE FROM schema_migrations",
	} {
		_, err := d.App.Exec(q)
		assert.ErrorContains(t, err, "permission denied", q)
	}
	var n int
	require.NoError(t, d.App.Get(&n, "SELECT count(*) FROM tenants"))
	assert.Positive(t, n)

	// a table a later migration adds (created by the owner) is usable without granting again
	_, err := d.Owner.Exec("CREATE TABLE later (tenant_id text NOT NULL REFERENCES tenants(id), id text, PRIMARY KEY (tenant_id, id)); SELECT rport_enable_rls('later')")
	require.NoError(t, err)
	require.NoError(t, postgres.InTenant(ctxFor("a"), d.App, func(tx *sqlx.Tx) error {
		_, err := tx.Exec("INSERT INTO later (tenant_id, id) VALUES ('a', '1')")
		return err
	}))
}

func TestInTenantRollsBackOnPanic(t *testing.T) {
	d := pgtest.New(t, "a")
	scratch(t, d)
	d.App.SetMaxOpenConns(1)
	assert.Panics(t, func() {
		_ = postgres.InTenant(ctxFor("a"), d.App, func(tx *sqlx.Tx) error {
			_, err := tx.Exec("INSERT INTO scratch (tenant_id, id) VALUES ('a', 'x')")
			require.NoError(t, err)
			panic("boom")
		})
	})
	// the only connection is usable again and the insert was rolled back
	var n int
	require.NoError(t, postgres.InTenant(ctxFor("a"), d.App, func(tx *sqlx.Tx) error {
		return tx.Get(&n, "SELECT count(*) FROM scratch")
	}))
	assert.Zero(t, n)
}

// A server for one organization keeps everything in the default tenant, so it must be usable (users can be added
// and log in there); _unresolved stays reserved.
func TestDefaultTenantIsActive(t *testing.T) {
	d := pgtest.New(t)
	var status string
	require.NoError(t, d.Owner.Get(&status, "SELECT status FROM tenants WHERE id = 'default'"))
	assert.Equal(t, "active", status)
	require.NoError(t, d.Owner.Get(&status, "SELECT status FROM tenants WHERE id = '_unresolved'"))
	assert.Equal(t, "reserved", status)
}
