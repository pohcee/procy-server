package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	errors2 "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/share/tenant"
)

// fakeKeys is a managed KeyProvider with a fixed key per tenant.
type fakeKeys struct {
	keys     map[tenant.ID][]byte
	locked   bool
	err      error
	keyCalls int
}

func (f *fakeKeys) Key(ctx context.Context) ([]byte, error) {
	f.keyCalls++
	if f.err != nil {
		return nil, f.err
	}
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	return f.keys[tid], nil
}

func (f *fakeKeys) Unlocked(context.Context) bool { return !f.locked }

func newFakeKeys() *fakeKeys {
	return &fakeKeys{keys: map[tenant.ID][]byte{
		"a":            bytes.Repeat([]byte{1}, KeySize),
		"b":            bytes.Repeat([]byte{2}, KeySize),
		tenant.Default: bytes.Repeat([]byte{3}, KeySize),
	}}
}

func newManagedSqliteManager(t *testing.T, keys KeyProvider) *Manager {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vault.sqlite.db")
	factory := NewStatefulDbProviderFactory(
		func() (DbProvider, error) { return openTestSqliteProvider(fileConfig(path), testLog) },
		&NotInitDbProvider{},
	)
	m := NewManagerWithKeyProvider(factory, &Aes256PassManager{}, keys, testLog)
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestManagedInitUnlockLockRefused(t *testing.T) {
	m := NewManagerWithKeyProvider(&DbProviderMock{}, &PassManagerMock{}, newFakeKeys(), testLog)
	ctx := ctxFor("a")
	for name, err := range map[string]error{
		"init":   m.Init(ctx, "12345678"),
		"unlock": m.UnLock(ctx, "12345678"),
		"lock":   m.Lock(ctx),
	} {
		var apiErr errors2.APIError
		require.True(t, errors.As(err, &apiErr), name)
		assert.Equal(t, http.StatusConflict, apiErr.HTTPStatus, name)
		assert.Equal(t, ErrManagedKey, err, name)
	}
	assert.ErrorIs(t, m.Init(context.Background(), "12345678"), tenant.ErrMissing)
	assert.False(t, m.UsesPassphrase())
	assert.True(t, NewManager(&DbProviderMock{}, &PassManagerMock{}, testLog).UsesPassphrase())
	assert.True(t, NewManagerWithKeyProvider(&DbProviderMock{}, &PassManagerMock{}, nil, testLog).UsesPassphrase())
	assert.True(t, NewManagerWithKeyProvider(&DbProviderMock{}, &PassManagerMock{}, NewPassphraseKeyProvider(), testLog).UsesPassphrase())
}

func TestManagedStatus(t *testing.T) {
	keys := newFakeKeys()
	// the status row is not read: a managed vault needs no initialisation
	m := NewManagerWithKeyProvider(&DbProviderMock{statusToGiveErr: errors.New("must not be read")}, &PassManagerMock{}, keys, testLog)
	st, err := m.Status(ctxFor("a"))
	require.NoError(t, err)
	out, err := json.Marshal(st)
	require.NoError(t, err)
	assert.JSONEq(t, `{"init":"setup-completed","status":"unlocked","managed":true}`, string(out))
	assert.False(t, m.IsLocked(ctxFor("a")))

	keys.locked = true
	st, err = m.Status(ctxFor("a"))
	require.NoError(t, err)
	assert.Equal(t, StatusLocked, st.LockStatus)
	assert.True(t, m.IsLocked(ctxFor("a")))

	_, err = m.Status(context.Background())
	assert.ErrorIs(t, err, tenant.ErrMissing)
}

func TestPassphraseStatusJSONUnchanged(t *testing.T) {
	m := NewManager(&DbProviderMock{statusToGive: DbStatus{StatusName: DbStatusInit}}, &PassManagerMock{}, testLog)
	m.passphrase.setPass(tenant.Default, "123")
	st, err := m.Status(ctxFor(tenant.Default))
	require.NoError(t, err)
	out, err := json.Marshal(st)
	require.NoError(t, err)
	assert.Equal(t, `{"init":"setup-completed","status":"unlocked"}`, string(out))
}

func TestManagedStoreInitialisesDB(t *testing.T) {
	keys := newFakeKeys()
	m := newManagedSqliteManager(t, keys)
	ctx := ctxFor("a")
	user := UserDataProviderMock{UsernameToGive: "u"}

	// a fresh server: the vault file does not exist yet and nobody called init
	id, err := m.Store(ctx, 0, input("c1", "k1", "top secret"), user)
	require.NoError(t, err)

	raw, found, err := m.dbFactory.GetDbProvider().GetByID(ctx, int(id.ID))
	require.NoError(t, err)
	require.True(t, found)
	assert.NotContains(t, raw.Value, "top secret")
	plain, err := decryptValue(raw.Value, keys.keys["a"])
	require.NoError(t, err)
	assert.Equal(t, "top secret", string(plain))

	got, found, err := m.GetOne(ctx, int(id.ID), user)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "top secret", got.Value)

	list, err := m.List(ctx, &http.Request{URL: mustURL(t, "/vault")})
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

func TestManagedLockedRefusesData(t *testing.T) {
	keys := newFakeKeys()
	keys.locked = true
	m := newManagedSqliteManager(t, keys)
	_, err := m.Store(ctxFor("a"), 0, input("c1", "k1", "v"), UserDataProviderMock{UsernameToGive: "u"})
	assert.Equal(t, errVaultLocked, err)
}

func TestManagedKeyErrorNothingWritten(t *testing.T) {
	keys := newFakeKeys()
	m := newManagedSqliteManager(t, keys)
	ctx := ctxFor("a")
	user := UserDataProviderMock{UsernameToGive: "u"}

	outage := errors2.APIError{Message: "key service unavailable", HTTPStatus: http.StatusServiceUnavailable}
	keys.err = outage
	_, err := m.Store(ctx, 0, input("c1", "k1", "v"), user)
	assert.Equal(t, outage, err)

	keys.err = nil
	keys.keys["a"] = []byte("too short")
	_, err = m.Store(ctx, 0, input("c1", "k2", "v"), user)
	assert.Equal(t, errManagedCrypto, err, "the cause is logged, not returned")

	list, err := m.List(ctx, &http.Request{URL: mustURL(t, "/vault")})
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestManagedTenantsUseOwnKeys(t *testing.T) {
	keys := newFakeKeys()
	m := newManagedSqliteManager(t, keys)
	user := UserDataProviderMock{UsernameToGive: "u"}
	id, err := m.Store(ctxFor("a"), 0, input("c1", "k1", "secret a"), user)
	require.NoError(t, err)

	raw, _, err := m.dbFactory.GetDbProvider().GetByID(ctxFor("a"), int(id.ID))
	require.NoError(t, err)
	_, err = decryptValue(raw.Value, keys.keys["b"])
	assert.Error(t, err, "b's key does not decrypt a's value")

	_, found, err := m.GetOne(ctxFor("b"), int(id.ID), user)
	require.NoError(t, err)
	assert.False(t, found, "b does not see a's value")
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

func TestManagedRefusesPassphraseVault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vault.sqlite.db")
	factory := NewStatefulDbProviderFactory(
		func() (DbProvider, error) { return openTestSqliteProvider(fileConfig(path), testLog) },
		&NotInitDbProvider{},
	)
	user := UserDataProviderMock{UsernameToGive: "u"}
	// tenant a was set up with a passphrase before the server got a key provider
	pm := NewManager(factory, &Aes256PassManager{}, testLog)
	require.NoError(t, pm.Init(ctxFor("a"), "passwordA1"))
	_, err := pm.Store(ctxFor("a"), 0, input("c1", "k1", "old secret"), user)
	require.NoError(t, err)

	m := NewManagerWithKeyProvider(factory, &Aes256PassManager{}, newFakeKeys(), testLog)
	defer m.Close()
	_, err = m.Store(ctxFor("a"), 0, input("c1", "k2", "new secret"), user)
	assert.Equal(t, ErrPassphraseVault, err)
	_, err = m.List(ctxFor("a"), &http.Request{URL: mustURL(t, "/vault")})
	assert.Equal(t, ErrPassphraseVault, err)
	// tenant b never had a passphrase
	_, err = m.Store(ctxFor("b"), 0, input("c1", "k1", "b secret"), user)
	assert.NoError(t, err)
}

func TestManagedDecryptFailureIsGeneric(t *testing.T) {
	keys := newFakeKeys()
	m := newManagedSqliteManager(t, keys)
	user := UserDataProviderMock{UsernameToGive: "u"}
	id, err := m.Store(ctxFor("a"), 0, input("c1", "k1", "v"), user)
	require.NoError(t, err)
	keys.keys["a"] = bytes.Repeat([]byte{9}, KeySize) // the provider's key changed
	_, _, err = m.GetOne(ctxFor("a"), int(id.ID), user)
	assert.Equal(t, errManagedCrypto, err)
}

func TestTypedNilPassphraseProviderIsDefault(t *testing.T) {
	m := NewManagerWithKeyProvider(&DbProviderMock{}, &PassManagerMock{}, (*PassphraseKeyProvider)(nil), testLog)
	assert.True(t, m.UsesPassphrase())
	assert.True(t, m.IsLocked(ctxFor("a")))
}
