package clients

import (
	"context"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ossclients "github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/repotest"
	"github.com/openrport/openrport/share/logger"

	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var testLog = logger.NewLogger("test", logger.LogOutput{}, logger.LogLevelError)

func TestClientStoreContract(t *testing.T) {
	repotest.ClientStore(t, func(t *testing.T, keep *time.Duration) ossclients.ClientStore {
		d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
		return New(d.App, keep)
	})
}

func TestRLSHidesOtherTenantsClients(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	s := New(d.App, nil)
	require.NoError(t, s.Save(repotest.Ctx(repotest.TenantA), repotest.StoreClient("c1", nil)))
	require.NoError(t, s.Save(repotest.Ctx(repotest.TenantB), repotest.StoreClient("c1", nil)))

	var tenants []string
	require.NoError(t, postgres.InTenant(repotest.Ctx(repotest.TenantA), d.App, func(tx *sqlx.Tx) error {
		return tx.Select(&tenants, "SELECT tenant_id FROM clients")
	}))
	assert.Equal(t, []string{"a"}, tenants)

	var n int
	require.NoError(t, d.App.Get(&n, "SELECT count(*) FROM clients"))
	assert.Zero(t, n)
}

func TestDeletingTenantDeletesClients(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	s := New(d.App, nil)
	require.NoError(t, s.Save(repotest.Ctx(repotest.TenantA), repotest.StoreClient("c1", nil)))
	require.NoError(t, s.Save(repotest.Ctx(repotest.TenantB), repotest.StoreClient("c1", nil)))
	_, err := d.Owner.Exec("DELETE FROM tenants WHERE id = 'a'")
	require.NoError(t, err)

	var tenants []string
	require.NoError(t, d.Owner.Select(&tenants, "SELECT tenant_id FROM clients"))
	assert.Equal(t, []string{"b"}, tenants)
}

// The details column holds exactly what the SQLite store writes, so rows copied from SQLite read back unchanged.
func TestDetailsAsSQLiteStoreWrites(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	s := New(d.App, nil)
	ctx := repotest.Ctx(repotest.TenantA)
	at := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)
	c := repotest.StoreClient("c1", &at)
	c.Labels = map[string]string{"k": "v"}
	require.NoError(t, s.Save(ctx, c))

	want, err := ossclients.EncodeStoredClient(c)
	require.NoError(t, err)
	var details string
	require.NoError(t, d.Owner.Get(&details, "SELECT details FROM clients WHERE id = 'c1'"))
	assert.Equal(t, want.Details, details)

	// a row as copied from SQLite
	_, err = d.Owner.Exec(`INSERT INTO clients (tenant_id, id, client_auth_id, disconnected_at, details)
		VALUES ('a', 'copied', 'auth', NULL, $1)`, `{"name":"copied","tags":["t"],"labels":null}`)
	require.NoError(t, err)
	all, err := s.GetAll(ctx, testLog)
	require.NoError(t, err)
	require.Len(t, all, 2)
	for _, got := range all {
		if got.GetID() == "copied" {
			assert.Equal(t, "copied", got.GetName())
			assert.Equal(t, []string{"t"}, got.Tags)
			assert.True(t, got.IsConnected())
		}
	}
}

func TestRequiresTenantBeforeDatabase(t *testing.T) {
	s := New(nil, nil) // a nil database would panic if touched
	_, err := s.GetAll(context.Background(), testLog)
	assert.Error(t, err)
}

func TestGet(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	s := New(d.App, nil)
	var _ ossclients.ClientGetter = s
	a, b := repotest.Ctx(repotest.TenantA), repotest.Ctx(repotest.TenantB)
	at := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)
	c := repotest.StoreClient("c1", &at)
	c.Labels = map[string]string{"k": "v"}
	c.AllowedUserGroups = []string{"g"}
	require.NoError(t, s.Save(a, c))

	got, err := s.Get(a, "c1", testLog)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "c1", got.GetID())
	assert.Equal(t, c.GetName(), got.GetName())
	assert.Equal(t, map[string]string{"k": "v"}, got.GetLabels())
	assert.Equal(t, []string{"g"}, got.GetAllowedUserGroups())
	require.NotNil(t, got.GetDisconnectedAt())
	assert.True(t, at.Equal(*got.GetDisconnectedAt()))

	got, err = s.Get(a, "missing", testLog)
	require.NoError(t, err)
	assert.Nil(t, got)
	got, err = s.Get(b, "c1", testLog)
	require.NoError(t, err)
	assert.Nil(t, got, "another tenant's client is not found")

	_, err = s.Get(context.Background(), "c1", testLog)
	assert.Error(t, err)
}

func TestGetLeavesOutObsolete(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	keep := time.Hour
	s := New(d.App, &keep)
	a := repotest.Ctx(repotest.TenantA)
	old := time.Now().Add(-2 * time.Hour)
	recent := time.Now().Add(-time.Minute)
	require.NoError(t, s.Save(a, repotest.StoreClient("old", &old)))
	require.NoError(t, s.Save(a, repotest.StoreClient("recent", &recent)))

	got, err := s.Get(a, "old", testLog)
	require.NoError(t, err)
	assert.Nil(t, got)
	got, err = s.Get(a, "recent", testLog)
	require.NoError(t, err)
	assert.NotNil(t, got)
}
