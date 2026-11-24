package importer

import (
	"context"
	"errors"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	vaultsmigration "github.com/openrport/openrport/db/migration/vaults"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	pgvault "github.com/openrport/openrport/server/storage/postgres/vault"
	"github.com/openrport/openrport/server/vault"
)

const vaultPass = "source-pass"

type vaultUser struct{}

func (vaultUser) GetGroups() []string { return []string{"Administrators", "ops"} }
func (vaultUser) GetUsername() string { return "admin" }

// fixedKeys is a managed key provider with one key for every tenant.
type fixedKeys struct{ key []byte }

func (k fixedKeys) Key(context.Context) ([]byte, error) { return k.key, nil }
func (fixedKeys) Unlocked(context.Context) bool         { return true }

var testKeys = fixedKeys{key: []byte("0123456789abcdef0123456789abcdef")}

// writeVault sets up the source vault with a passphrase, as the API does, and stores values; it returns their ids
// with their plaintext.
func writeVault(t *testing.T, d *dataDir) map[int]string {
	t.Helper()
	out := map[int]string{}
	d.with("vault.sqlite.db", vaultsmigration.FS, func(db *sqlx.DB) {
		factory := vault.NewStatefulDbProviderFactory(func() (vault.DbProvider, error) { return vault.NewSqliteProvider(db, testLog), nil }, &vault.NotInitDbProvider{})
		m := vault.NewManager(factory, &vault.Aes256PassManager{}, testLog)
		require.NoError(t, m.Init(ctxDefault, vaultPass))
		for _, v := range []vault.InputValue{
			{Key: "db-password", Value: "s3cret", Type: vault.SecretType},
			{ClientID: "c1", Key: "notes", Value: "**hello**", Type: vault.MarkdownType, RequiredGroup: "ops"},
		} {
			plain := v.Value
			id, err := m.Store(ctxDefault, 0, &v, vaultUser{})
			require.NoError(t, err)
			out[int(id.ID)] = plain
		}
	})
	return out
}

func TestImportVaultPassphraseCopied(t *testing.T) {
	pg := pgtest.New(t)
	d := newDataDir(t)
	values := writeVault(t, d)

	rep := run(t, pg, Options{From: d.dir})
	want(t, rep, entVaultValues, 2, 2, 0, 0)
	want(t, rep, entVaultStatus, 1, 1, 0, 0)

	// the target vault unlocks with the source passphrase and reads the values
	m := vault.NewManager(pgvault.NewFactory(pgvault.New(pg.App)), &vault.Aes256PassManager{}, testLog)
	require.Error(t, m.UnLock(ctxDefault, "wrong-pass"))
	require.NoError(t, m.UnLock(ctxDefault, vaultPass))
	for id, plain := range values {
		got, found, err := m.GetOne(ctxDefault, id, vaultUser{})
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, plain, got.Value)
	}
	// a new value gets an id above the imported ones and reads back
	newID, err := m.Store(ctxDefault, 0, &vault.InputValue{Key: "later", Value: "v", Type: vault.TextType}, vaultUser{})
	require.NoError(t, err)
	for id := range values {
		assert.Greater(t, int(newID.ID), id)
	}

	// a second run finds everything present
	rep = run(t, pg, Options{From: d.dir})
	want(t, rep, entVaultValues, 2, 0, 2, 0)
	want(t, rep, entVaultStatus, 1, 0, 1, 0)
}

func TestImportVaultReencrypted(t *testing.T) {
	pg := pgtest.New(t)
	d := newDataDir(t)
	values := writeVault(t, d)

	rep := run(t, pg, Options{From: d.dir, VaultKeys: testKeys, VaultPassphrase: vaultPass})
	want(t, rep, entVaultValues, 2, 2, 0, 0)
	want(t, rep, entVaultStatus, 1, 0, 0, 1)
	var n int
	require.NoError(t, pg.Owner.Get(&n, "SELECT count(*) FROM vault_status"))
	assert.Zero(t, n, "no passphrase status may be written for a managed vault")

	m := vault.NewManagerWithKeyProvider(pgvault.NewFactory(pgvault.New(pg.App)), &vault.Aes256PassManager{}, testKeys, testLog)
	for id, plain := range values {
		got, found, err := m.GetOne(ctxDefault, id, vaultUser{})
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, plain, got.Value)
	}
	rep = run(t, pg, Options{From: d.dir, VaultKeys: testKeys, VaultPassphrase: vaultPass})
	want(t, rep, entVaultValues, 2, 0, 2, 0)
}

func TestImportVaultNeedsPassphrase(t *testing.T) {
	pg := pgtest.New(t)
	d := newDataDir(t)
	writeVault(t, d)
	for _, pass := range []string{"", "wrong-pass"} {
		_, err := Run(context.Background(), pg.App, Options{From: d.dir, VaultKeys: testKeys, VaultPassphrase: pass})
		require.Error(t, err, pass)
		if pass == "" {
			assert.Contains(t, err.Error(), "passphrase is needed")
		} else {
			assert.Contains(t, err.Error(), "does not match")
		}
	}
	var n int
	require.NoError(t, pg.Owner.Get(&n, "SELECT count(*) FROM vault_values"))
	assert.Zero(t, n)

	// a target vault set up with another passphrase is refused
	other := newDataDir(t)
	other.with("vault.sqlite.db", vaultsmigration.FS, func(db *sqlx.DB) {
		factory := vault.NewStatefulDbProviderFactory(func() (vault.DbProvider, error) { return vault.NewSqliteProvider(db, testLog), nil }, &vault.NotInitDbProvider{})
		require.NoError(t, vault.NewManager(factory, &vault.Aes256PassManager{}, testLog).Init(ctxDefault, "another-pass"))
	})
	run(t, pg, Options{From: other.dir})
	_, err := Run(context.Background(), pg.App, Options{From: d.dir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "another passphrase")
	// and a managed import into it as well
	_, err = Run(context.Background(), pg.App, Options{From: d.dir, VaultKeys: testKeys, VaultPassphrase: vaultPass})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "set up with a passphrase")
}

// noKeys fails the test if the target vault key is asked for.
type noKeys struct{ t *testing.T }

func (k noKeys) Key(context.Context) ([]byte, error) {
	k.t.Error("a dry run must not ask for (and so create) the target vault key")
	return nil, errors.New("no key")
}
func (noKeys) Unlocked(context.Context) bool { return true }

// A dry run into a managed vault checks the passphrase and every value but doesn't get the tenant's key: a key
// service would create and store it.
func TestImportVaultDryRunGetsNoKey(t *testing.T) {
	pg := pgtest.New(t)
	d := newDataDir(t)
	writeVault(t, d)
	rep := run(t, pg, Options{From: d.dir, VaultKeys: noKeys{t}, VaultPassphrase: vaultPass, DryRun: true})
	want(t, rep, entVaultValues, 2, 2, 0, 0)
	_, err := Run(context.Background(), pg.App, Options{From: d.dir, VaultKeys: noKeys{t}, VaultPassphrase: "wrong-pass", DryRun: true})
	assert.ErrorContains(t, err, "does not match")
}
