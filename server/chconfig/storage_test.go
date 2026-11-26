package chconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rportplus "github.com/pohcee/procy-server/plus"
	"github.com/pohcee/procy-server/plus/capabilities/oauth"
	"github.com/pohcee/procy-server/share/logger"
)

const testDSN = "postgres://rport_app@db.example.com/rport"

// postgresConfig is a minimal valid config of a Postgres node: no client or API auth source, the vault managed by
// a local key file (the passphrase vault is refused on Postgres).
func postgresConfig() *Config {
	server := defaultValidMinServerConfig
	server.Auth = ""
	return &Config{
		Server:  server,
		Storage: StorageConfig{Backend: "postgres", DSN: testDSN},
		Vault:   VaultConfig{KeyProvider: "local", LocalKeyFile: "/etc/rport/vault.key"},
	}
}

func validate(c *Config) error {
	mLog := logger.NewMemLogger()
	return c.ParseAndValidate(&mLog)
}

func TestStorageDefaultsToSQLite(t *testing.T) {
	c := &Config{Server: defaultValidMinServerConfig}
	require.NoError(t, validate(c))
	assert.False(t, c.Storage.IsPostgres())
	assert.Equal(t, "", c.Storage.Backend, "no [storage]: nothing is filled in")
	assert.Equal(t, KeyProviderPassphrase, c.Vault.KeyProvider)

	c = &Config{Server: defaultValidMinServerConfig, Storage: StorageConfig{Backend: "sqlite"}}
	require.NoError(t, validate(c))
	assert.False(t, c.Storage.IsPostgres())

	// SQLite still needs a client auth source, as before
	server := defaultValidMinServerConfig
	server.Auth = ""
	err := validate(&Config{Server: server})
	assert.ErrorContains(t, err, "client authentication must be enabled")

	for name, s := range map[string]StorageConfig{
		"dsn":      {DSN: testDSN},
		"dsn_file": {DSNFile: "/run/secrets/dsn"},
		"role":     {Role: "api"},
	} {
		err := validate(&Config{Server: defaultValidMinServerConfig, Storage: s})
		assert.ErrorContains(t, err, "[storage] "+name+` is set but [storage] backend is not "postgres"`, name)
	}

	err = validate(&Config{Server: defaultValidMinServerConfig, Storage: StorageConfig{Backend: "mysql"}})
	assert.ErrorContains(t, err, `invalid [storage] backend "mysql": expected "sqlite" or "postgres"`)
}

func TestStoragePostgresNeedsOneDSN(t *testing.T) {
	c := postgresConfig()
	c.Storage.DSN = ""
	assert.ErrorContains(t, validate(c), `[storage] backend = "postgres" needs dsn or dsn_file`)

	c = postgresConfig()
	c.Storage.DSNFile = "/run/secrets/dsn"
	assert.ErrorContains(t, validate(c), "[storage] dsn and dsn_file are both set: expected only one of them")

	c = postgresConfig()
	require.NoError(t, validate(c))
	assert.True(t, c.Storage.IsPostgres())
	assert.Equal(t, testDSN, c.Storage.DSN)
	assert.Equal(t, StorageRoleAll, c.Storage.Role)
}

func TestStorageDSNFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dsn")
	require.NoError(t, os.WriteFile(path, []byte("  "+testDSN+"\n"), 0600))
	c := postgresConfig()
	c.Storage.DSN = ""
	c.Storage.DSNFile = path
	require.NoError(t, validate(c))
	assert.Equal(t, testDSN, c.Storage.DSN, "read and trimmed")

	c = postgresConfig()
	c.Storage.DSN = ""
	c.Storage.DSNFile = filepath.Join(t.TempDir(), "missing")
	assert.ErrorContains(t, validate(c), "[storage] dsn_file")

	empty := filepath.Join(t.TempDir(), "empty")
	require.NoError(t, os.WriteFile(empty, []byte("\n"), 0600))
	c = postgresConfig()
	c.Storage.DSN = ""
	c.Storage.DSNFile = empty
	assert.ErrorContains(t, validate(c), "is empty")
}

func TestStorageRoles(t *testing.T) {
	c := postgresConfig()
	c.Storage.Role = "worker"
	assert.ErrorContains(t, validate(c), `invalid [storage] role "worker": expected "all", "api" or "device"`)

	c = postgresConfig()
	c.Storage.Role = "api"
	require.NoError(t, validate(c))
	assert.Equal(t, StorageRoleAPI, c.Storage.Role)
}

func TestStorageRoleDeviceRefusesAPIAddress(t *testing.T) {
	c := postgresConfig()
	c.Storage.Role = "device"
	c.API.Address = "0.0.0.0:3000"
	assert.ErrorContains(t, validate(c), `[api] address must be empty`)

	c = deviceConfig()
	require.NoError(t, validate(c))
	assert.Equal(t, StorageRoleDevice, c.Storage.Role)
}

// deviceConfig is a valid config of a device process: no API, no vault.
func deviceConfig() *Config {
	c := postgresConfig()
	c.Storage.Role = "device"
	c.API.Address = ""
	c.Vault = VaultConfig{}
	return c
}

func TestStorageRoleDeviceRefusesVault(t *testing.T) {
	c := deviceConfig()
	c.Vault.KeyProvider = KeyProviderLocal
	c.Vault.LocalKeyFile = "/k"
	assert.ErrorContains(t, validate(c), `[vault] is not used with [storage] role = "device"`)
}

func TestStoragePostgresRefusesAuthFileAndDatabase(t *testing.T) {
	for _, tc := range []struct {
		key string
		set func(c *Config)
	}{
		{"[server] auth", func(c *Config) { c.Server.Auth = "client:secret" }},
		{"[server] auth_file", func(c *Config) { c.Server.AuthFile = "/etc/rport/client-auth.json" }},
		{"[server] auth_table", func(c *Config) { c.Server.AuthTable = "clients_auth" }},
		{"[api] auth", func(c *Config) { c.API.Auth = "admin:secret" }},
		{"[api] auth_file", func(c *Config) { c.API.AuthFile = "/etc/rport/api-auth.json" }},
		{"[api] auth_user_table", func(c *Config) { c.API.AuthUserTable = "users" }},
		{"[api] auth_group_table", func(c *Config) { c.API.AuthGroupTable = "groups" }},
		{"[api] auth_group_details_table", func(c *Config) { c.API.AuthGroupDetailsTable = "group_details" }},
		{"[database] db_type", func(c *Config) { c.Database.Type = "sqlite" }},
		{"[database] db_host", func(c *Config) { c.Database.Host = "localhost" }},
		{"[database] db_user", func(c *Config) { c.Database.User = "rport" }},
		{"[database] db_password", func(c *Config) { c.Database.Password = "secret" }},
		{"[database] db_name", func(c *Config) { c.Database.Name = "rport" }},
	} {
		t.Run(tc.key, func(t *testing.T) {
			c := postgresConfig()
			tc.set(c)
			assert.EqualError(t, validate(c), tc.key+` is not used with [storage] backend = "postgres": users and client credentials are stored in Postgres`)
		})
	}
}

func TestStoragePostgresNeedsNoAuthSource(t *testing.T) {
	c := postgresConfig()
	c.API.Address = "127.0.0.1:3000"
	c.API.JWTSecret = "0123456789abcdef0123456789abcdef"
	require.NoError(t, validate(c))
	assert.Empty(t, c.Server.Auth)
	assert.Empty(t, c.API.Auth)
}

func TestVaultKMSNeedsPostgres(t *testing.T) {
	for _, v := range []VaultConfig{
		{KeyProvider: "kms", KMSKey: "projects/p/locations/l/keyRings/r/cryptoKeys/k"},
		{KeyProvider: "local", LocalKeyFile: "/etc/rport/vault.key"},
	} {
		c := &Config{Server: defaultValidMinServerConfig, Vault: v}
		assert.ErrorContains(t, validate(c), `[vault] key_provider = "`+v.KeyProvider+`" needs [storage] backend = "postgres": the wrapped vault keys are stored in Postgres`)
	}

	c := postgresConfig()
	c.Vault = VaultConfig{KeyProvider: "hsm"}
	assert.ErrorContains(t, validate(c), `invalid [vault] key_provider "hsm": expected "passphrase", "kms" or "local"`)
}

func TestVaultKMSNeedsKey(t *testing.T) {
	c := postgresConfig()
	c.Vault = VaultConfig{KeyProvider: "kms"}
	assert.ErrorContains(t, validate(c), `[vault] key_provider = "kms" needs kms_key`)

	c = postgresConfig()
	c.Vault = VaultConfig{KeyProvider: "local"}
	assert.ErrorContains(t, validate(c), `[vault] key_provider = "local" needs local_key_file`)

	c = postgresConfig()
	c.Vault = VaultConfig{KeyProvider: "kms", KMSKey: "projects/p/locations/l/keyRings/r/cryptoKeys/k"}
	require.NoError(t, validate(c))

	c = postgresConfig()
	c.Vault.KMSKey = "projects/p/locations/l/keyRings/r/cryptoKeys/k"
	assert.ErrorContains(t, validate(c), `[vault] kms_key is set but key_provider is not "kms"`)

	c = &Config{Server: defaultValidMinServerConfig, Vault: VaultConfig{KMSKey: "projects/p/locations/l/keyRings/r/cryptoKeys/k"}}
	assert.ErrorContains(t, validate(c), `[vault] kms_key is set but key_provider is not "kms"`)
}

// A login by proxy header or Plus OAuth names no tenant the server could look up yet, so it would land in the
// reserved tenant; on Postgres both are refused until logins pick their organization.
func TestStoragePostgresRefusesHeaderAndOAuthLogin(t *testing.T) {
	c := postgresConfig()
	c.API.AuthHeader = "X-Remote-User"
	assert.EqualError(t, validate(c), `[api] auth_header is not yet supported with [storage] backend = "postgres"`)

	c = postgresConfig()
	c.PlusConfig.PluginConfig = &rportplus.PluginConfig{PluginPath: "/usr/local/lib/rport/rport-plus.so"}
	c.PlusConfig.OAuthConfig = &oauth.Config{}
	assert.EqualError(t, validate(c), `[plus-oauth] is not yet supported with [storage] backend = "postgres"`)
}

const passphraseRefused = `[vault] key_provider = "passphrase" is not supported with [storage] backend = "postgres": several nodes can share the database and the passphrase unlocks only one; set key_provider = "kms" or "local"`

// Several nodes can share a Postgres database while the passphrase unlocks the vault of one process only, so the
// passphrase vault is refused on Postgres, whether chosen by default or explicitly.
func TestStoragePostgresRefusesPassphraseVault(t *testing.T) {
	for _, provider := range []string{"", "passphrase"} {
		c := postgresConfig()
		c.Vault = VaultConfig{KeyProvider: provider}
		assert.EqualError(t, validate(c), passphraseRefused, "key_provider %q", provider)
	}

	c := postgresConfig()
	require.NoError(t, validate(c))
	assert.Equal(t, KeyProviderLocal, c.Vault.KeyProvider)
}

func TestStoragePostgresNeedsJWTSecret(t *testing.T) {
	c := postgresConfig()
	c.Server.DataDir = t.TempDir()
	c.API.Address = "127.0.0.1:3000"
	assert.EqualError(t, validate(c), `[api] jwt_secret is required with [storage] backend = "postgres": every node must sign and check API tokens with the same secret`)
	_, err := os.Stat(filepath.Join(c.Server.DataDir, jwtSecretFileName))
	assert.True(t, os.IsNotExist(err), "no secret is generated into the data dir")

	c = postgresConfig()
	c.API.Address = "127.0.0.1:3000"
	c.API.JWTSecret = "0123456789abcdef0123456789abcdef"
	require.NoError(t, validate(c))

	c = postgresConfig()
	require.NoError(t, validate(c), "no API, no secret needed")
	assert.Empty(t, c.API.JWTSecret)
}

// TestGeneratedJWTSecretIsPersistedAndReused (config_security_test.go) covers SQLite: the secret is still generated.
func TestStorageSQLiteStillGeneratesJWTSecret(t *testing.T) {
	cfg := apiConfigWith(t.TempDir(), APIConfig{})
	require.NoError(t, validate(&cfg))
	assert.NotEmpty(t, cfg.API.JWTSecret)
}

func TestClusterNodeIDDefaultsToHostname(t *testing.T) {
	host, err := os.Hostname()
	require.NoError(t, err)
	c := postgresConfig()
	require.NoError(t, validate(c))
	assert.Equal(t, host, c.Cluster.NodeID)

	c = postgresConfig()
	c.Cluster.NodeID = "node-2.eu_west"
	require.NoError(t, validate(c))
	assert.Equal(t, "node-2.eu_west", c.Cluster.NodeID, "kept as set")

	prev := osHostname
	t.Cleanup(func() { osHostname = prev })
	osHostname = func() (string, error) { return "", errors.New("no hostname") }
	c = postgresConfig()
	assert.ErrorContains(t, validate(c), "[cluster] node_id is empty and the hostname can't be read: no hostname")
	osHostname = func() (string, error) { return "", nil }
	c = postgresConfig()
	assert.ErrorContains(t, validate(c), "[cluster] node_id is empty and the hostname is empty")
}

func TestClusterNodeIDInvalid(t *testing.T) {
	for _, id := range []string{"-node", ".node", "node 1", "node/1", "nöde", strings.Repeat("a", 64)} {
		c := postgresConfig()
		c.Cluster.NodeID = id
		assert.ErrorContains(t, validate(c), "invalid [cluster] node_id", id)
	}
	for _, id := range []string{"a", "Node_1", "n.example-2", strings.Repeat("a", 63)} {
		c := postgresConfig()
		c.Cluster.NodeID = id
		assert.NoError(t, validate(c), id)
	}

	prev := osHostname
	t.Cleanup(func() { osHostname = prev })
	osHostname = func() (string, error) { return "bad host", nil }
	c := postgresConfig()
	assert.ErrorContains(t, validate(c), `invalid [cluster] node_id "bad host" (the hostname)`)
}

func TestClusterNodeIDNeedsPostgres(t *testing.T) {
	c := &Config{Server: defaultValidMinServerConfig, Cluster: ClusterConfig{NodeID: "node-1"}}
	assert.EqualError(t, validate(c), `[cluster] node_id is set but [storage] backend is not "postgres"`)

	c = &Config{Server: defaultValidMinServerConfig}
	require.NoError(t, validate(c))
	assert.Empty(t, c.Cluster.NodeID, "standalone: no node id")
}

const emptyKeySeedWarning = "[server] key_seed is empty: the node's fingerprint changes on each start; nodes sharing a database should use the same key_seed"

// infoLogged validates c and returns what it logged.
func infoLogged(t *testing.T, c *Config) string {
	t.Helper()
	mLog := logger.NewMemLogger()
	require.NoError(t, c.ParseAndValidate(&mLog))
	f, err := os.CreateTemp(t.TempDir(), "log")
	require.NoError(t, err)
	defer f.Close()
	mLog.Flush(logger.NewLogger("test", logger.LogOutput{File: f}, logger.LogLevelDebug))
	b, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	return string(b)
}

func TestPostgresWarnsEmptyKeySeed(t *testing.T) {
	assert.Contains(t, infoLogged(t, postgresConfig()), emptyKeySeedWarning)

	c := postgresConfig()
	c.Server.KeySeed = "a-shared-key-seed-0123456789"
	assert.NotContains(t, infoLogged(t, c), "key_seed is empty")

	c = postgresConfig()
	c.Storage.Role = "api"
	assert.NotContains(t, infoLogged(t, c), "key_seed is empty", "no client listener")

	assert.NotContains(t, infoLogged(t, &Config{Server: defaultValidMinServerConfig}), "key_seed is empty", "standalone")
}
