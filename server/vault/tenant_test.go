package vault

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/db/migration/vaults"
	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/tenant"
)

func ctxFor(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

type fileConfig string

func (c fileConfig) GetVaultDBPath() string { return string(c) }

// preTenantVersion is the last vaults.db schema version without tenant_id.
const preTenantVersion = 1

func migrateTo(t *testing.T, path string, version uint) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("sqlite3", path)
	require.NoError(t, err)
	require.NoError(t, sqlite.MigrateTo(db, vaults.FS, version))
	return db
}

func newMemProvider(t *testing.T) *SqliteProvider {
	t.Helper()
	p, err := openTestSqliteProvider(configMock{}, testLog)
	require.NoError(t, err)
	t.Cleanup(func() { p.Close() })
	return p
}

func input(clientID, key, value string) *InputValue {
	return &InputValue{ClientID: clientID, Key: key, Value: value, Type: TextType}
}

func TestSqliteProviderRequiresTenant(t *testing.T) {
	p := newMemProvider(t)
	ctx := context.Background()
	_, err := p.GetStatus(ctx)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.ErrorIs(t, p.SetStatus(ctx, DbStatus{StatusName: DbStatusInit}), tenant.ErrMissing)
	_, _, err = p.GetByID(ctx, 1)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.List(ctx, &query.ListOptions{})
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, _, err = p.FindByKeyAndClientID(ctx, "k", "c")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.Save(ctx, "u", 0, input("c", "k", "v"), time.Now())
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.ErrorIs(t, p.Delete(ctx, 1), tenant.ErrMissing)
}

func TestSqliteProviderIsolatesTenants(t *testing.T) {
	p := newMemProvider(t)
	a, b := ctxFor("a"), ctxFor("b")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// status per tenant
	require.NoError(t, p.SetStatus(a, DbStatus{StatusName: DbStatusInit, EncCheckValue: "ea", DecCheckValue: "da"}))
	st, err := p.GetStatus(b)
	require.NoError(t, err)
	assert.Equal(t, "", st.StatusName)
	require.NoError(t, p.SetStatus(b, DbStatus{StatusName: DbStatusInit, EncCheckValue: "eb", DecCheckValue: "db"}))
	require.NoError(t, p.SetStatus(b, DbStatus{StatusName: DbStatusInit, EncCheckValue: "eb2", DecCheckValue: "db2"}))
	st, err = p.GetStatus(a)
	require.NoError(t, err)
	assert.Equal(t, "ea", st.EncCheckValue)
	st, err = p.GetStatus(b)
	require.NoError(t, err)
	assert.Equal(t, "eb2", st.EncCheckValue)

	// the same client and key in both tenants
	idA, err := p.Save(a, "u", 0, input("c1", "k1", "va"), now)
	require.NoError(t, err)
	idB, err := p.Save(b, "u", 0, input("c1", "k1", "vb"), now)
	require.NoError(t, err)

	got, found, err := p.FindByKeyAndClientID(b, "k1", "c1")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "vb", got.Value)
	_, found, err = p.GetByID(b, int(idA))
	require.NoError(t, err)
	assert.False(t, found)

	_, err = p.Save(a, "u", 0, input("c2", "k2", "va2"), now)
	require.NoError(t, err)
	listA, err := p.List(a, &query.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, listA, 2)
	listB, err := p.List(b, &query.ListOptions{Filters: []query.FilterOption{{
		Column: []string{"key"}, Operator: query.FilterOperatorTypeEQ, Values: []string{"k1"},
	}}})
	require.NoError(t, err)
	require.Len(t, listB, 1)
	assert.EqualValues(t, idB, listB[0].ID)

	// b can't update or delete a's value
	_, err = p.Save(b, "u", idA, input("c1", "k1", "hacked"), now)
	require.NoError(t, err)
	gotA, found, err := p.GetByID(a, int(idA))
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "va", gotA.Value)
	assert.Error(t, p.Delete(b, int(idA)))
	_, found, err = p.GetByID(a, int(idA))
	require.NoError(t, err)
	assert.True(t, found)

	require.NoError(t, p.Delete(a, int(idA)))
	_, found, err = p.GetByID(b, int(idB))
	require.NoError(t, err)
	assert.True(t, found)
}

func TestVaultJSONHasNoTenant(t *testing.T) {
	p := newMemProvider(t)
	ctx := ctxFor("a")
	id, err := p.Save(ctx, "u", 0, input("c", "k", "v"), time.Now())
	require.NoError(t, err)
	val, _, err := p.GetByID(ctx, int(id))
	require.NoError(t, err)
	list, err := p.List(ctx, &query.ListOptions{})
	require.NoError(t, err)
	for _, x := range []interface{}{val, list} {
		out, err := json.Marshal(x)
		require.NoError(t, err)
		assert.NotContains(t, string(out), "tenant")
	}
}

func TestManagerLocksPerTenant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.sqlite.db")
	factory := NewStatefulDbProviderFactory(
		func() (DbProvider, error) { return openTestSqliteProvider(fileConfig(path), testLog) },
		&NotInitDbProvider{},
	)
	m := NewManager(factory, &Aes256PassManager{}, testLog)
	defer m.Close()
	a, b := ctxFor("a"), ctxFor("b")
	user := UserDataProviderMock{UsernameToGive: "u"}

	require.NoError(t, m.Init(a, "passwordA1"))
	st, err := m.Status(b)
	require.NoError(t, err)
	assert.Equal(t, DbStatusNotInit, st.InitStatus, "b is not initialized by a's init")

	require.NoError(t, m.Init(b, "passwordB1"))
	_, err = m.Store(a, 0, input("c1", "k1", "secret a"), user)
	require.NoError(t, err)
	idB, err := m.Store(b, 0, input("c1", "k1", "secret b"), user)
	require.NoError(t, err)

	require.NoError(t, m.Lock(a))
	assert.True(t, m.IsLocked(a))
	assert.False(t, m.IsLocked(b))
	st, err = m.Status(b)
	require.NoError(t, err)
	assert.Equal(t, StatusUnlocked, st.LockStatus)

	got, found, err := m.GetOne(b, int(idB.ID), user)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "secret b", got.Value)

	// each tenant's passphrase only unlocks its own vault
	assert.ErrorIs(t, m.UnLock(a, "passwordB1"), WrongPasswordError)
	require.NoError(t, m.UnLock(a, "passwordA1"))
	assert.False(t, m.IsLocked(a))
}

func TestManagerRequiresTenant(t *testing.T) {
	m := NewManager(&DbProviderMock{statusToGive: DbStatus{StatusName: DbStatusInit}}, &PassManagerMock{PassMatchToGive: true}, testLog)
	ctx := context.Background()
	assert.ErrorIs(t, m.Init(ctx, "12345678"), tenant.ErrMissing)
	assert.ErrorIs(t, m.UnLock(ctx, "12345678"), tenant.ErrMissing)
	assert.ErrorIs(t, m.Lock(ctx), tenant.ErrMissing)
	_, err := m.Status(ctx)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.True(t, m.IsLocked(ctx))
}

func TestUpgradeKeepsVaultInDefaultTenant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.sqlite.db")
	old := migrateTo(t, path, preTenantVersion)
	_, err := old.Exec("INSERT INTO `status` (`db_status`, `enc_check`, `dec_check`) VALUES ('setup-completed', 'enc', 'dec')")
	require.NoError(t, err)
	require.NoError(t, addDemoData(old))
	// a deleted value bumped the id sequence past the live rows
	_, err = old.Exec("INSERT INTO `values` (`client_id`, `key`, `value`, `created_at`, `updated_at`, `created_by`, `type`) VALUES ('c9', 'k9', 'v9', '2001-01-01T00:00:00Z', '2001-01-01T00:00:00Z', 'u', 'text')")
	require.NoError(t, err)
	_, err = old.Exec("DELETE FROM `values` WHERE `key` = 'k9'")
	require.NoError(t, err)
	require.NoError(t, old.Close())

	p, err := openTestSqliteProvider(fileConfig(path), testLog)
	require.NoError(t, err)
	defer p.Close()
	ctx := ctxFor(tenant.Default)

	st, err := p.GetStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, DbStatusInit, st.StatusName)
	assert.Equal(t, "enc", st.EncCheckValue)

	list, err := p.List(ctx, &query.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, list, 2)
	v, found, err := p.FindByKeyAndClientID(ctx, "key1", "client1")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 1, v.ID)
	assert.Equal(t, "val1", v.Value)

	// the unique (client, key) rule still holds
	_, err = p.Save(ctx, "u", 0, input("client1", "key1", "x"), time.Now())
	assert.ErrorContains(t, err, "UNIQUE constraint failed")
	// ids are not reused
	id, err := p.Save(ctx, "u", 0, input("c9", "k9", "x"), time.Now())
	require.NoError(t, err)
	assert.EqualValues(t, 4, id)
}

func TestDowngradeKeepsOnlyDefaultVault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.sqlite.db")
	p, err := openTestSqliteProvider(fileConfig(path), testLog)
	require.NoError(t, err)
	for _, tid := range []tenant.ID{tenant.Default, "a"} {
		ctx := ctxFor(tid)
		require.NoError(t, p.SetStatus(ctx, DbStatus{StatusName: DbStatusInit, EncCheckValue: string(tid)}))
		_, err := p.Save(ctx, "u", 0, input("c1", "k1", string(tid)), time.Now())
		require.NoError(t, err)
	}
	require.NoError(t, p.Close())

	down := migrateTo(t, path, preTenantVersion)
	defer down.Close()
	var values []string
	require.NoError(t, down.Select(&values, "SELECT `value` FROM `values`"))
	assert.Equal(t, []string{"default"}, values)
	var enc []string
	require.NoError(t, down.Select(&enc, "SELECT `enc_check` FROM `status`"))
	assert.Equal(t, []string{"default"}, enc)
	for _, table := range []string{"values", "status"} {
		var n int
		require.NoError(t, down.Get(&n, "SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'tenant_id'", table))
		assert.Zero(t, n, table)
	}
}
