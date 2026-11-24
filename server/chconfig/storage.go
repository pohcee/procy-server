package chconfig

import (
	"errors"
	"fmt"
	"os"
	"strings"

	rportplus "github.com/openrport/openrport/plus"
)

// Storage backends ([storage] backend).
const (
	StorageBackendSQLite   = "sqlite"
	StorageBackendPostgres = "postgres"
)

// Storage roles ([storage] role) of a node on Postgres.
const (
	// StorageRoleAll serves the API and the client listener.
	StorageRoleAll = "all"
	// StorageRoleAPI serves the API only: no client listener, work on clients is refused as "not connected".
	StorageRoleAPI = "api"
	// StorageRoleDevice is reserved for a node that serves only the client listener.
	StorageRoleDevice = "device"
)

// Vault key providers ([vault] key_provider).
const (
	// KeyProviderPassphrase is the vault unlocked with a passphrase after each start, as standalone.
	KeyProviderPassphrase = "passphrase"
	// KeyProviderKMS wraps each tenant's vault key with a Cloud KMS key.
	KeyProviderKMS = "kms"
	// KeyProviderLocal wraps each tenant's vault key with a key read from a local file.
	KeyProviderLocal = "local"
)

// StorageConfig is the [storage] section: where the server keeps its data. Without it the server uses the SQLite
// databases in the data directory.
type StorageConfig struct {
	// Backend is "sqlite" (default) or "postgres".
	Backend string `mapstructure:"backend"`
	// DSN is the Postgres connection URL of the application role. After validation it holds the URL read from
	// DSNFile when that is set.
	DSN string `mapstructure:"dsn"`
	// DSNFile is a file holding the DSN (surrounding whitespace is ignored).
	DSNFile string `mapstructure:"dsn_file"`
	// Role is what a Postgres node serves: "all" (default) or "api".
	Role string `mapstructure:"role"`
}

// IsPostgres reports whether the server keeps its data in Postgres.
func (s StorageConfig) IsPostgres() bool {
	return s.Backend == StorageBackendPostgres
}

// VaultConfig is the [vault] section: how the vault's encryption keys are protected.
type VaultConfig struct {
	// KeyProvider is "passphrase" (default), "kms" or "local".
	KeyProvider string `mapstructure:"key_provider"`
	// KMSKey is the Cloud KMS CryptoKey resource name (projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>).
	KMSKey string `mapstructure:"kms_key"`
	// LocalKeyFile is a file holding a 32-byte key, hex or base64 encoded.
	LocalKeyFile string `mapstructure:"local_key_file"`
}

const postgresStoresCredentials = `is not used with [storage] backend = "postgres": users and client credentials are stored in Postgres`

func (c *Config) parseAndValidateStorage() error {
	s := &c.Storage
	switch s.Backend {
	case "", StorageBackendSQLite:
		for _, kv := range [][2]string{{"dsn", s.DSN}, {"dsn_file", s.DSNFile}, {"role", s.Role}} {
			if kv[1] != "" {
				return fmt.Errorf(`[storage] %s is set but [storage] backend is not "postgres"`, kv[0])
			}
		}
	case StorageBackendPostgres:
		if err := s.parseAndValidatePostgres(); err != nil {
			return err
		}
		if err := c.refuseLocalAuthSources(); err != nil {
			return err
		}
	default:
		return fmt.Errorf(`invalid [storage] backend %q: expected "sqlite" or "postgres"`, s.Backend)
	}
	return c.parseAndValidateVault()
}

func (s *StorageConfig) parseAndValidatePostgres() error {
	switch {
	case s.DSN != "" && s.DSNFile != "":
		return fmt.Errorf("[storage] dsn and dsn_file are both set: expected only one of them")
	case s.DSN == "" && s.DSNFile == "":
		return fmt.Errorf(`[storage] backend = "postgres" needs dsn or dsn_file`)
	case s.DSNFile != "":
		b, err := os.ReadFile(s.DSNFile)
		if err != nil {
			return fmt.Errorf("[storage] dsn_file: %w", err)
		}
		s.DSN = strings.TrimSpace(string(b))
		if s.DSN == "" {
			return fmt.Errorf("[storage] dsn_file %s is empty", s.DSNFile)
		}
	}

	switch s.Role {
	case "":
		s.Role = StorageRoleAll
	case StorageRoleAll, StorageRoleAPI:
	case StorageRoleDevice:
		return fmt.Errorf("storage role %q is not yet supported", s.Role)
	default:
		return fmt.Errorf(`invalid [storage] role %q: expected "all" or "api"`, s.Role)
	}
	return nil
}

// refuseLocalAuthSources refuses the user, client credential and database settings a Postgres node would ignore.
func (c *Config) refuseLocalAuthSources() error {
	for _, kv := range [][2]string{
		{"[server] auth", c.Server.Auth},
		{"[server] auth_file", c.Server.AuthFile},
		{"[server] auth_table", c.Server.AuthTable},
		{"[api] auth", c.API.Auth},
		{"[api] auth_file", c.API.AuthFile},
		{"[api] auth_user_table", c.API.AuthUserTable},
		{"[api] auth_group_table", c.API.AuthGroupTable},
		{"[api] auth_group_details_table", c.API.AuthGroupDetailsTable},
		{"[database] db_type", c.Database.Type},
		{"[database] db_host", c.Database.Host},
		{"[database] db_user", c.Database.User},
		{"[database] db_password", c.Database.Password},
		{"[database] db_name", c.Database.Name},
	} {
		if kv[1] != "" {
			return fmt.Errorf("%s %s", kv[0], postgresStoresCredentials)
		}
	}
	// these logins name no tenant the server can look up yet: the user would land in the reserved tenant of
	// unresolved requests
	if c.API.AuthHeader != "" {
		return errors.New(`[api] auth_header is not yet supported with [storage] backend = "postgres"`)
	}
	if rportplus.IsPlusOAuthEnabled(c.PlusConfig) {
		return errors.New(`[plus-oauth] is not yet supported with [storage] backend = "postgres"`)
	}
	return nil
}

func (c *Config) parseAndValidateVault() error {
	v := &c.Vault
	switch v.KeyProvider {
	case "":
		v.KeyProvider = KeyProviderPassphrase
	case KeyProviderPassphrase:
	case KeyProviderKMS:
		if v.KMSKey == "" {
			return fmt.Errorf(`[vault] key_provider = "kms" needs kms_key`)
		}
	case KeyProviderLocal:
		if v.LocalKeyFile == "" {
			return fmt.Errorf(`[vault] key_provider = "local" needs local_key_file`)
		}
	default:
		return fmt.Errorf(`invalid [vault] key_provider %q: expected "passphrase", "kms" or "local"`, v.KeyProvider)
	}
	if v.KMSKey != "" && v.KeyProvider != KeyProviderKMS {
		return fmt.Errorf(`[vault] kms_key is set but key_provider is not "kms"`)
	}
	if v.LocalKeyFile != "" && v.KeyProvider != KeyProviderLocal {
		return fmt.Errorf(`[vault] local_key_file is set but key_provider is not "local"`)
	}
	if v.KeyProvider != KeyProviderPassphrase && !c.Storage.IsPostgres() {
		return fmt.Errorf(`[vault] key_provider = %q needs [storage] backend = "postgres": the wrapped vault keys are stored in Postgres`, v.KeyProvider)
	}
	return nil
}
