package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/cmd/rportd/usercli"
	"github.com/pohcee/procy-server/server/storage/postgres"
	"github.com/pohcee/procy-server/server/storage/postgres/migrate"
	"github.com/pohcee/procy-server/server/storage/postgres/pgserver"
	"github.com/pohcee/procy-server/server/storage/postgres/pgtest"
	pgusers "github.com/pohcee/procy-server/server/storage/postgres/users"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/tenant"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// writeConfig makes conf the config file of the commands for the test.
func writeConfig(t *testing.T, conf string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rportd.conf")
	require.NoError(t, os.WriteFile(path, []byte(conf), 0600))
	prev := *cfgPath
	*cfgPath = path
	t.Cleanup(func() { *cfgPath = prev })
}

// writePostgresConfig writes the config of a Postgres node connecting with dsn, its vault managed by a local key file.
func writePostgresConfig(t *testing.T, dsn string) {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "vault.key")
	require.NoError(t, os.WriteFile(keyFile, []byte(strings.Repeat("ab", 32)+"\n"), 0600))
	writeConfig(t, "[server]\n"+
		"  data_dir = \""+t.TempDir()+"\"\n"+
		"[storage]\n"+
		"  backend = \"postgres\"\n"+
		"  dsn = \""+dsn+"\"\n"+
		"[vault]\n"+
		"  key_provider = \"local\"\n"+
		"  local_key_file = \""+strings.ReplaceAll(keyFile, `\`, `\\`)+"\"\n")
}

func setFlag(t *testing.T, flag *string, value string) {
	t.Helper()
	prev := *flag
	*flag = value
	t.Cleanup(func() { *flag = prev })
}

// commandOutput returns a logger for a command and a func returning what it wrote.
func commandOutput(t *testing.T) (*logger.Logger, func() string) {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "out.log"))
	require.NoError(t, err)
	t.Cleanup(func() { f.Close() })
	return logger.NewLogger("test", logger.LogOutput{File: f}, logger.LogLevelInfo), func() string {
		b, err := os.ReadFile(f.Name())
		require.NoError(t, err)
		return string(b)
	}
}

func TestMigratePostgres(t *testing.T) {
	d := pgtest.NewEmpty(t)
	writePostgresConfig(t, d.AppURL)
	setFlag(t, migrateDSN, d.OwnerURL)
	setFlag(t, migrateGrantTo, pgtest.AppRole)
	setFlag(t, migrateDevice, pgtest.DeviceRole)

	out, written := commandOutput(t)
	require.NoError(t, runMigrate(out))
	assert.Regexp(t, `applied [1-9][0-9]* migration\(s\)`, written())
	assert.Contains(t, written(), "granted the application privileges to "+pgtest.AppRole)
	assert.Contains(t, written(), "granted the device privileges to "+pgtest.DeviceRole)
	require.NoError(t, postgres.CheckDeviceRole(context.Background(), d.Device))

	// the application role can now run a node
	ctx := context.Background()
	require.NoError(t, migrate.Check(ctx, d.App))
	require.NoError(t, postgres.CheckAppRole(ctx, d.App))

	out, written = commandOutput(t)
	require.NoError(t, runMigrate(out))
	assert.Contains(t, written(), "nothing to migrate")
}

func TestMigratePostgresDefaultsToStorageDSN(t *testing.T) {
	d := pgtest.NewEmpty(t)
	writePostgresConfig(t, d.OwnerURL)
	out, written := commandOutput(t)
	require.NoError(t, runMigrate(out))
	assert.Contains(t, written(), "migration(s)")
	assert.NotContains(t, written(), "granted")
}

func TestMigrateDSNNeedsPostgres(t *testing.T) {
	writeMigrateConfig(t, filepath.Join(t.TempDir(), "data"))
	setFlag(t, migrateDSN, "postgres://localhost/rport")
	out, _ := commandOutput(t)
	assert.ErrorContains(t, runMigrate(out), `only with [storage] backend = "postgres"`)
}

func TestTenantAdd(t *testing.T) {
	d := pgtest.New(t)
	writePostgresConfig(t, d.AppURL)
	setFlag(t, tenantDSN, d.OwnerURL)

	out, written := commandOutput(t)
	require.NoError(t, runTenantAdd(out, []string{"acme", "Acme Corp"}))
	assert.Contains(t, written(), `added tenant "acme"`)
	var name string
	require.NoError(t, d.Owner.Get(&name, "SELECT name FROM tenants WHERE id = 'acme'"))
	assert.Equal(t, "Acme Corp", name)

	out, _ = commandOutput(t)
	assert.ErrorContains(t, runTenantAdd(out, []string{"default"}), "reserved")
	assert.Error(t, runTenantAdd(out, []string{"acme"}), "already exists")
}

func TestTenantAddRefusesSQLite(t *testing.T) {
	writeMigrateConfig(t, filepath.Join(t.TempDir(), "data"))
	out, _ := commandOutput(t)
	assert.ErrorContains(t, runTenantAdd(out, []string{"acme"}), `tenants exist only with [storage] backend = "postgres"`)
}

type fixedPassword string

func (p fixedPassword) ReadPassword() (string, error) { return string(p), nil }

func TestUserAddPostgres(t *testing.T) {
	d := pgtest.New(t)
	ctx := context.Background()
	require.NoError(t, pgserver.AddTenant(ctx, d.Owner, "acme", ""))
	writePostgresConfig(t, d.AppURL)
	require.NoError(t, loadCommandConfig(memLog()))

	pw := fixedPassword("correct horse battery staple")
	svc, err := usercli.NewUserService(cfg, "acme")
	require.NoError(t, err)
	require.NoError(t, usercli.CreateUser(svc, pw, "carol", []string{"Administrators"}, ""))

	store := pgusers.New(d.App)
	u, err := store.GetByUsername(tenant.WithID(ctx, "acme"), "carol")
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.Equal(t, []string{"Administrators"}, u.Groups)
	u, err = store.GetByUsername(tenant.WithID(ctx, tenant.Default), "carol")
	require.NoError(t, err)
	assert.Nil(t, u, "only in the tenant given")

	// the default tenant by default
	svc, err = usercli.NewUserService(cfg, "")
	require.NoError(t, err)
	require.NoError(t, usercli.CreateUser(svc, pw, "dave", nil, ""))
	u, err = store.GetByUsername(tenant.WithID(ctx, tenant.Default), "dave")
	require.NoError(t, err)
	assert.NotNil(t, u)

	require.NoError(t, usercli.DeleteUser(svc, "dave"))

	_, err = usercli.NewUserService(cfg, "nope")
	assert.ErrorContains(t, err, `tenant "nope"`)
}

func TestUserTenantNeedsPostgres(t *testing.T) {
	writeConfig(t, "[server]\n  auth = \"client:secret\"\n  data_dir = \""+t.TempDir()+"\"\n[api]\n  auth_file = \""+
		strings.ReplaceAll(filepath.Join(t.TempDir(), "users.json"), `\`, `\\`)+"\"\n")
	require.NoError(t, loadCommandConfig(memLog()))
	_, err := usercli.NewUserService(cfg, "acme")
	assert.ErrorContains(t, err, `--tenant is used only with [storage] backend = "postgres"`)
}

func memLog() *logger.MemLogger {
	l := logger.NewMemLogger()
	return &l
}

func TestImportAndTenantAddRefuseRoot(t *testing.T) {
	asRoot(t)
	writePostgresConfig(t, "postgres://rport_app@127.0.0.1:1/none")
	setFlag(t, importFrom, t.TempDir())
	err := runImport(context.Background(), strings.NewReader(""), io.Discard, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "root")
	err = runTenantAdd(logger.NewLogger("tenant", logger.LogOutput{File: os.Stdout}, logger.LogLevelInfo), []string{"acme"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "root")
}
