package vault_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/repotest"
	oss "github.com/openrport/openrport/server/vault"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"

	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	"github.com/openrport/openrport/server/storage/postgres/vault"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestContract(t *testing.T) {
	repotest.Vault(t, func(t *testing.T) oss.DbProvider {
		d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
		return vault.New(d.App)
	})
}

var t0 = time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

var tables = []string{"vault_values", "vault_status"}

func seed(t *testing.T, d *pgtest.DB) {
	t.Helper()
	s := vault.New(d.App)
	for _, id := range []tenant.ID{repotest.TenantA, repotest.TenantB} {
		ctx := repotest.Ctx(id)
		_, err := s.Save(ctx, "u", 0, &oss.InputValue{ClientID: "c", Key: "k", Value: string(id), Type: oss.SecretType}, t0)
		require.NoError(t, err)
		require.NoError(t, s.SetStatus(ctx, oss.DbStatus{StatusName: oss.DbStatusInit, EncCheckValue: "e", DecCheckValue: "d"}))
	}
}

func TestRLSBackstop(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	seed(t, d)
	for _, table := range tables {
		var tenants []string
		require.NoError(t, postgres.InTenant(repotest.Ctx(repotest.TenantA), d.App, func(tx *sqlx.Tx) error {
			return tx.Select(&tenants, "SELECT DISTINCT tenant_id FROM "+table)
		}))
		assert.Equal(t, []string{"a"}, tenants, table)
		var n int
		require.NoError(t, d.App.Get(&n, "SELECT count(*) FROM "+table))
		assert.Zero(t, n, table)
	}
}

func TestTenantDeleteCascades(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	seed(t, d)
	_, err := d.Owner.Exec("DELETE FROM tenants WHERE id = 'a'")
	require.NoError(t, err)
	for _, table := range tables {
		var tenants []string
		require.NoError(t, d.Owner.Select(&tenants, "SELECT DISTINCT tenant_id FROM "+table))
		assert.Equal(t, []string{"b"}, tenants, table)
	}
}

func TestIDsAreGlobalAndNeverReused(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	s := vault.New(d.App)
	a, b := repotest.Ctx(repotest.TenantA), repotest.Ctx(repotest.TenantB)
	in := &oss.InputValue{ClientID: "c", Key: "k", Value: "v", Type: oss.SecretType}
	idA, err := s.Save(a, "u", 0, in, t0)
	require.NoError(t, err)
	idB, err := s.Save(b, "u", 0, in, t0)
	require.NoError(t, err)
	assert.Greater(t, idB, idA)
	require.NoError(t, s.Delete(a, int(idA)))
	again, err := s.Save(a, "u", 0, in, t0)
	require.NoError(t, err)
	assert.Greater(t, again, idB)
}

func TestImportedNullColumnsRead(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	_, err := d.Owner.Exec(`INSERT INTO vault_values (tenant_id, id, client_id, required_group, created_at, created_by,
		updated_at, updated_by, key, value, type) VALUES ('a', 7, 'c', NULL, $1, 'u', $1, NULL, 'k', 'v', 'text')`, t0)
	require.NoError(t, err)
	_, err = d.Owner.Exec(`INSERT INTO vault_status (tenant_id, db_status) VALUES ('a', 'setup-completed')`)
	require.NoError(t, err)
	s := vault.New(d.App)
	a := repotest.Ctx(repotest.TenantA)
	v, found, err := s.GetByID(a, 7)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "", v.RequiredGroup)
	assert.Nil(t, v.UpdatedBy)
	st, err := s.GetStatus(a)
	require.NoError(t, err)
	assert.Equal(t, oss.DbStatus{ID: st.ID, StatusName: oss.DbStatusInit}, st)
}

func TestListFiltersCreatedAtAsSQLiteText(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	s := vault.New(d.App)
	a := repotest.Ctx(repotest.TenantA)
	for i, k := range []string{"k1", "k2"} {
		_, err := s.Save(a, "u", 0, &oss.InputValue{Key: k, Type: oss.TextType}, t0.Add(time.Duration(i)*time.Hour))
		require.NoError(t, err)
	}
	vals, err := s.List(a, &query.ListOptions{Filters: []query.FilterOption{
		{Column: []string{"created_at"}, Operator: query.FilterOperatorTypeGT, Values: []string{"2026-01-01T10:30:00Z"}},
	}})
	require.NoError(t, err)
	require.Len(t, vals, 1)
	assert.Equal(t, "k2", vals[0].Key)
	vals, err = s.List(a, &query.ListOptions{Filters: []query.FilterOption{
		{Column: []string{"created_at"}, Values: []string{"2026-01-01T10:00:00Z"}},
	}})
	require.NoError(t, err)
	require.Len(t, vals, 1)
	assert.Equal(t, "k1", vals[0].Key)
}

func TestCloseLeavesDBOpen(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	require.NoError(t, vault.New(d.App).Close())
	require.NoError(t, d.App.PingContext(context.Background()))
}

type keys struct{}

func (keys) Key(ctx context.Context) ([]byte, error) {
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	return bytes.Repeat([]byte(tid)[:1], oss.KeySize), nil
}

func (keys) Unlocked(context.Context) bool { return true }

type user struct{}

func (user) GetGroups() []string { return []string{"Administrators"} }
func (user) GetUsername() string { return "admin" }

var testLog = logger.NewLogger("vault", logger.LogOutput{File: os.Stdout}, logger.LogLevelError)

// The factory with a managed key provider: the vault needs no initialisation, values round-trip per tenant.
func TestFactoryManagedVault(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	m := oss.NewManagerWithKeyProvider(vault.NewFactory(vault.New(d.App)), &oss.Aes256PassManager{}, keys{}, testLog)
	t.Cleanup(func() { _ = m.Close() })
	a, b := repotest.Ctx(repotest.TenantA), repotest.Ctx(repotest.TenantB)

	st, err := m.Status(a)
	require.NoError(t, err)
	assert.Equal(t, oss.StatusReport{InitStatus: oss.DbStatusInit, LockStatus: oss.StatusUnlocked, Managed: true}, st)

	in := &oss.InputValue{ClientID: "c", Key: "k", Value: "secret A", Type: oss.SecretType}
	idA, err := m.Store(a, 0, in, user{})
	require.NoError(t, err)
	in.Value = "secret B"
	_, err = m.Store(b, 0, in, user{})
	require.NoError(t, err)

	got, found, err := m.GetOne(a, int(idA.ID), user{})
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "secret A", got.Value)
	_, found, err = m.GetOne(b, int(idA.ID), user{})
	require.NoError(t, err)
	assert.False(t, found)

	list, err := m.List(b, httptest.NewRequest("GET", "/vault?sort=-key", nil))
	require.NoError(t, err)
	require.Len(t, list, 1)

	var stored string
	require.NoError(t, d.Owner.Get(&stored, "SELECT value FROM vault_values WHERE id = $1", idA.ID))
	assert.NotContains(t, stored, "secret", "stored encrypted")
}

// The factory with passphrase vaults: each tenant's vault is initialised and unlocked on its own.
func TestFactoryPassphraseVault(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	m := oss.NewManager(vault.NewFactory(vault.New(d.App)), &oss.Aes256PassManager{}, testLog)
	a, b := repotest.Ctx(repotest.TenantA), repotest.Ctx(repotest.TenantB)

	require.NoError(t, m.Init(a, "passphrase-a"))
	st, err := m.Status(b)
	require.NoError(t, err)
	assert.Equal(t, oss.DbStatusNotInit, st.InitStatus)

	idA, err := m.Store(a, 0, &oss.InputValue{Key: "k", Value: "v", Type: oss.TextType}, user{})
	require.NoError(t, err)
	require.NoError(t, m.Lock(a))
	require.NoError(t, m.UnLock(a, "passphrase-a"))
	got, found, err := m.GetOne(a, int(idA.ID), user{})
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "v", got.Value)

	_, err = m.Store(b, 0, &oss.InputValue{Key: "k", Value: "v", Type: oss.TextType}, user{})
	assert.Error(t, err, "B's vault is not initialised")
}
