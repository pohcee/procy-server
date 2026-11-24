package chserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/migration"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/cgroups"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

var fixedSQLiteFiles = map[string]string{
	"clients.db":       "clients",
	"jobs.db":          "jobs",
	"client_groups.db": "client_groups",
	"monitoring.db":    "monitoring",
	"notifications.db": "notifications",
	"library.db":       "library",
	"api_token.db":     "api_token",
	"api_sessions.db":  "api_sessions",
}

func storageTestConfig(t *testing.T) *chconfig.Config {
	t.Helper()
	return &chconfig.Config{
		Server: chconfig.ServerConfig{
			DataDir:      t.TempDir(),
			Auth:         "client:secret",
			AuthID:       "client",
			AuthPassword: "secret",
		},
		API: chconfig.APIConfig{
			Auth: "admin:foobaz",
		},
		Logging: chconfig.LogConfig{
			LogOutput: logger.LogOutput{File: os.Stdout},
			LogLevel:  logger.LogLevelDebug,
		},
	}
}

func storageTestCtx() context.Context {
	return tenant.WithID(context.Background(), tenant.Default)
}

func migrationSet(t *testing.T, name string) migration.Set {
	t.Helper()
	for _, s := range migration.All() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no migration set %q", name)
	return migration.Set{}
}

func requireMigrated(t *testing.T, file, set string) {
	t.Helper()
	db, err := sqlite.Open(file, sqlite.DataSourceOptions{})
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, sqlite.CheckVersion(db, migrationSet(t, set).FS), file)
}

func openTestRepositories(t *testing.T, config *chconfig.Config, given *Repositories, skip bool) (*Repositories, error) {
	t.Helper()
	repos, storage, err := openSQLiteRepositories(storageTestCtx(), config, given, skip, testLog)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { assert.NoError(t, storage.closeAll()) })
	return repos, nil
}

func TestMigrateSQLiteCreatesEveryFixedDatabase(t *testing.T) {
	for _, auditLog := range []bool{false, true} {
		config := storageTestConfig(t)
		config.API.AuditLog.Enable = auditLog
		dir := config.Server.DataDir

		require.NoError(t, MigrateSQLite(config, testLog))

		for file, set := range fixedSQLiteFiles {
			requireMigrated(t, filepath.Join(dir, file), set)
		}
		assert.NoFileExists(t, filepath.Join(dir, chconfig.DefaultVaultDBName), "migrate must not create the vault")
		if auditLog {
			requireMigrated(t, filepath.Join(dir, "auditlog.db"), "auditlog")
		} else {
			assert.NoFileExists(t, filepath.Join(dir, "auditlog.db"))
		}

		// a second run changes nothing
		require.NoError(t, MigrateSQLite(config, testLog))
	}
}

func TestMigrateSQLiteMigratesAnExistingVault(t *testing.T) {
	config := storageTestConfig(t)
	vaultPath := filepath.Join(config.Server.DataDir, chconfig.DefaultVaultDBName)
	db, err := sqlite.Open(vaultPath, sqlite.DataSourceOptions{})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	require.NoError(t, MigrateSQLite(config, testLog))

	requireMigrated(t, vaultPath, "vaults")
}

func TestSkipMigrationsRefusesUnmigratedDir(t *testing.T) {
	config := storageTestConfig(t)

	_, err := openTestRepositories(t, config, nil, true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "clients.db")
	assert.Contains(t, err.Error(), `rportd migrate`)
	assert.NoFileExists(t, filepath.Join(config.Server.DataDir, "clients.db"), "skip must not create files")
}

func TestSkipMigrationsRefusesBehindAuditLog(t *testing.T) {
	config := storageTestConfig(t)
	require.NoError(t, MigrateSQLite(config, testLog))
	db, err := sqlite.Open(filepath.Join(config.Server.DataDir, "auditlog.db"), sqlite.DataSourceOptions{})
	require.NoError(t, err)
	require.NoError(t, db.Close())
	config.API.AuditLog.Enable = true

	_, err = openTestRepositories(t, config, nil, true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "auditlog.db")
	assert.Contains(t, err.Error(), `rportd migrate`)
}

func TestSkipMigrationsRefusesMissingAuditLog(t *testing.T) {
	config := storageTestConfig(t)
	config.API.AuditLog.Enable = true
	require.NoError(t, MigrateSQLite(config, testLog))
	auditLogPath := filepath.Join(config.Server.DataDir, "auditlog.db")
	require.NoError(t, os.Remove(auditLogPath))

	_, err := openTestRepositories(t, config, nil, true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "auditlog.db: database does not exist")
	assert.Contains(t, err.Error(), `rportd migrate`)
	assert.NoFileExists(t, auditLogPath, "skip must not create the audit log")
}

func TestSkipMigrationsAfterMigrate(t *testing.T) {
	config := storageTestConfig(t)
	config.API.AuditLog.Enable = true
	require.NoError(t, MigrateSQLite(config, testLog))

	repos, err := openTestRepositories(t, config, nil, true)

	require.NoError(t, err)
	assert.NotNil(t, repos.Clients)
	assert.NotNil(t, repos.AuditLog)
	assert.NotNil(t, repos.Users)
	assert.NotNil(t, repos.ClientAuth)
}

func TestVaultCreatedAndMigratedOnInit(t *testing.T) {
	for _, skip := range []bool{false, true} {
		config := storageTestConfig(t)
		require.NoError(t, MigrateSQLite(config, testLog))
		vaultPath := filepath.Join(config.Server.DataDir, chconfig.DefaultVaultDBName)

		repos, err := openTestRepositories(t, config, nil, skip)
		require.NoError(t, err)
		assert.NoFileExists(t, vaultPath)

		require.NoError(t, repos.Vault.Init())
		t.Cleanup(func() { _ = repos.Vault.GetDbProvider().Close() })
		requireMigrated(t, vaultPath, "vaults")
	}
}

type fakeClientGroups struct {
	cgroups.ClientGroupProvider
}

func TestInjectedRepositoryIsUsed(t *testing.T) {
	config := storageTestConfig(t)
	fake := &fakeClientGroups{}

	repos, err := openTestRepositories(t, config, &Repositories{ClientGroups: fake}, false)

	require.NoError(t, err)
	assert.Same(t, fake, repos.ClientGroups)
	assert.NotNil(t, repos.Clients, "the other domains get SQLite")
	assert.NoFileExists(t, filepath.Join(config.Server.DataDir, "client_groups.db"))
	assert.FileExists(t, filepath.Join(config.Server.DataDir, "clients.db"))
}

func TestInjectedSharedFileIsNotOpened(t *testing.T) {
	config := storageTestConfig(t)
	dir := config.Server.DataDir
	src := openRepos(t)
	given := &Repositories{Clients: src.Clients, StoredTunnels: src.StoredTunnels, Jobs: src.Jobs}

	repos, err := openTestRepositories(t, config, given, false)

	require.NoError(t, err)
	assert.Same(t, given.Clients, repos.Clients)
	assert.NoFileExists(t, filepath.Join(dir, "clients.db"), "clients and stored tunnels are both injected")
	assert.FileExists(t, filepath.Join(dir, "jobs.db"), "schedules still need jobs.db")
	assert.NotNil(t, repos.Schedules)
}

// openRepos opens a SQLite repository set in a directory of its own, as a source of repositories to inject.
func openRepos(t *testing.T) *Repositories {
	t.Helper()
	repos, err := openTestRepositories(t, storageTestConfig(t), nil, false)
	require.NoError(t, err)
	return repos
}

func TestNewServerOnExistingDataDir(t *testing.T) {
	config := storageTestConfig(t)
	config.API.AuditLog.Enable = true

	repos, storage, err := openSQLiteRepositories(storageTestCtx(), config, nil, false, testLog)
	require.NoError(t, err)
	require.NoError(t, repos.ClientGroups.Create(storageTestCtx(), &cgroups.ClientGroup{ID: "g1", Params: &cgroups.ClientParams{}}))
	require.NoError(t, storage.closeAll())

	repos, err = openTestRepositories(t, config, nil, false)
	require.NoError(t, err)
	group, err := repos.ClientGroups.Get(storageTestCtx(), "g1")
	require.NoError(t, err)
	require.NotNil(t, group)

	// the dir is at the latest version: skipping migrations works on it too
	_, err = openTestRepositories(t, config, nil, true)
	require.NoError(t, err)
}
