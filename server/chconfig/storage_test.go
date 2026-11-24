package chconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rportplus "github.com/openrport/openrport/plus"
	"github.com/openrport/openrport/plus/capabilities/oauth"
	"github.com/openrport/openrport/share/logger"
)

const testDSN = "postgres://rport_app@db.example.com/rport"

// postgresConfig is a minimal valid config of a Postgres node: no client or API auth source.
func postgresConfig() *Config {
	server := defaultValidMinServerConfig
	server.Auth = ""
	return &Config{
		Server:  server,
		Storage: StorageConfig{Backend: "postgres", DSN: testDSN},
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

func TestStorageRoleDeviceNotSupported(t *testing.T) {
	c := postgresConfig()
	c.Storage.Role = "device"
	assert.ErrorContains(t, validate(c), `storage role "device" is not yet supported`)

	c = postgresConfig()
	c.Storage.Role = "worker"
	assert.ErrorContains(t, validate(c), `invalid [storage] role "worker": expected "all" or "api"`)

	c = postgresConfig()
	c.Storage.Role = "api"
	require.NoError(t, validate(c))
	assert.Equal(t, StorageRoleAPI, c.Storage.Role)
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
	c.Vault = VaultConfig{KeyProvider: "passphrase"}
	require.NoError(t, validate(c), "a single Postgres node may use the passphrase vault")

	c = postgresConfig()
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
	c.Vault = VaultConfig{KeyProvider: "passphrase", KMSKey: "projects/p/locations/l/keyRings/r/cryptoKeys/k"}
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
