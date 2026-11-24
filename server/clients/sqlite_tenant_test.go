package clients

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	bindata "github.com/golang-migrate/migrate/v4/source/go_bindata"
	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/migration/clients"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/share/tenant"
)

func ctxFor(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

var defaultCtx = ctxFor(tenant.Default)

// migrateClientsDBTo opens a file DB and migrates it to the given schema version (up or down).
func migrateClientsDBTo(t *testing.T, path string, version uint) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("sqlite3", path)
	require.NoError(t, err)
	src, err := bindata.WithInstance(bindata.Resource(clients.AssetNames(), clients.Asset))
	require.NoError(t, err)
	drv, err := sqlite3.WithInstance(db.DB, &sqlite3.Config{})
	require.NoError(t, err)
	m, err := migrate.NewWithInstance("go-bindata", src, "sqlite3", drv)
	require.NoError(t, err)
	err = m.Migrate(version)
	if err != migrate.ErrNoChange {
		require.NoError(t, err)
	}
	return db
}

func TestClientStoreRequiresTenant(t *testing.T) {
	p := NewFakeClientProvider(t, nil)
	defer p.Close()
	ctx := context.Background()
	c := New(t).Logger(testLog).Build()

	_, err := p.GetAll(ctx, testLog)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.get(ctx, c.GetID(), testLog)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.ErrorIs(t, p.Save(ctx, c), tenant.ErrMissing)
	assert.ErrorIs(t, p.Delete(ctx, c.GetID(), testLog), tenant.ErrMissing)
	assert.ErrorIs(t, p.DeleteObsolete(ctx, testLog), tenant.ErrMissing)
}

func TestClientStoreIsolatesTenants(t *testing.T) {
	keep := hour
	p := NewFakeClientProvider(t, &keep)
	defer p.Close()
	a, b := ctxFor("a"), ctxFor("b")

	ca := New(t).ID("same-id").ClientAuthID("from a").Logger(testLog).Build()
	cb := New(t).ID("same-id").ClientAuthID("from b").Logger(testLog).Build()
	require.NoError(t, p.Save(a, ca))
	require.NoError(t, p.Save(b, cb))

	got, err := p.get(a, "same-id", testLog)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "from a", got.GetClientAuthID())

	all, err := p.GetAll(b, testLog)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "from b", all[0].GetClientAuthID())

	// obsolete clients in tenant a are deleted only in tenant a
	oa := New(t).ID("obsolete").DisconnectedDuration(keep + time.Minute).Logger(testLog).Build()
	ob := New(t).ID("obsolete").DisconnectedDuration(keep + time.Minute).Logger(testLog).Build()
	require.NoError(t, p.Save(a, oa))
	require.NoError(t, p.Save(b, ob))
	require.NoError(t, p.DeleteObsolete(a, testLog))
	gone, err := p.get(a, "obsolete", testLog)
	require.NoError(t, err)
	assert.Nil(t, gone)
	still, err := p.get(b, "obsolete", testLog)
	require.NoError(t, err)
	assert.NotNil(t, still)

	require.NoError(t, p.Delete(a, "same-id", testLog))
	gone, err = p.get(a, "same-id", testLog)
	require.NoError(t, err)
	assert.Nil(t, gone)
	still, err = p.get(b, "same-id", testLog)
	require.NoError(t, err)
	require.NotNil(t, still)
	assert.Equal(t, "from b", still.GetClientAuthID())
}

func TestClientStoreUpgradeKeepsDataInDefaultTenant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clients.db")
	old := migrateClientsDBTo(t, path, 3)
	_, err := old.Exec(`INSERT INTO clients (id, client_auth_id, disconnected_at, details) VALUES ('legacy', 'auth1', '2020-01-01 00:00:00', '{"name":"old"}')`)
	require.NoError(t, err)
	_, err = old.Exec(`INSERT INTO stored_tunnels (id, client_id, name) VALUES ('t1', 'legacy', 'old tunnel')`)
	require.NoError(t, err)
	require.NoError(t, old.Close())

	db, err := sqlite.New(path, clients.AssetNames(), clients.Asset, DataSourceOptions)
	require.NoError(t, err)
	defer db.Close()
	p := newSqliteProvider(db, nil)

	got, err := p.get(defaultCtx, "legacy", testLog)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "old", got.GetName())
	assert.Equal(t, "auth1", got.GetClientAuthID())

	var tunnelTenant string
	require.NoError(t, db.Get(&tunnelTenant, `SELECT tenant_id FROM stored_tunnels WHERE id = 't1'`))
	assert.Equal(t, "default", tunnelTenant)
}

func TestClientStoreDowngradeKeepsOnlyDefaultTenant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clients.db")
	db, err := sqlite.New(path, clients.AssetNames(), clients.Asset, DataSourceOptions)
	require.NoError(t, err)
	p := newSqliteProvider(db, nil)
	require.NoError(t, p.Save(defaultCtx, New(t).ID("c1").ClientAuthID("default").Logger(testLog).Build()))
	require.NoError(t, p.Save(ctxFor("a"), New(t).ID("c1").ClientAuthID("a").Logger(testLog).Build()))
	_, err = db.Exec(`INSERT INTO stored_tunnels (tenant_id, id, client_id, name) VALUES ('default', 't1', 'c1', 'default'), ('a', 't1', 'c1', 'a')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	down := migrateClientsDBTo(t, path, 3)
	defer down.Close()

	var ids []string
	require.NoError(t, down.Select(&ids, `SELECT id FROM clients`))
	assert.Equal(t, []string{"c1"}, ids)
	var authID string
	require.NoError(t, down.Get(&authID, `SELECT client_auth_id FROM clients WHERE id = 'c1'`))
	assert.Equal(t, "default", authID)

	var names []string
	require.NoError(t, down.Select(&names, `SELECT name FROM stored_tunnels`))
	assert.Equal(t, []string{"default"}, names)

	for _, table := range []string{"clients", "stored_tunnels"} {
		var n int
		require.NoError(t, down.Get(&n, `SELECT COUNT(*) FROM pragma_table_info('`+table+`') WHERE name = 'tenant_id'`))
		assert.Zero(t, n, table)
	}
}
