package pgserver

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/jmoiron/sqlx"

	chserver "github.com/openrport/openrport/server"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/migrate"
	"github.com/openrport/openrport/server/vault"
	"github.com/openrport/openrport/server/vault/vaultkms"
	"github.com/openrport/openrport/share/files"
	"github.com/openrport/openrport/share/logger"
)

// localKeyName is the key name stored with the vault keys wrapped by a local key file. The local key file KMS
// ignores it; it only tells the wrapped keys apart from those of a Cloud KMS key.
const localKeyName = "local-key-file"

// Open connects to the Postgres database of a node configured with [storage] backend = "postgres".
func Open(ctx context.Context, cfg *chconfig.Config) (*sqlx.DB, error) {
	if !cfg.Storage.IsPostgres() {
		return nil, errors.New(`pgserver: [storage] backend is not "postgres"`)
	}
	return postgres.Open(ctx, cfg.Storage.DSN)
}

// OpenOwner connects to the Postgres database of a node as the schema owner, for migrating it and adding tenants:
// with ownerDSN, or with [storage] dsn when ownerDSN is empty.
func OpenOwner(ctx context.Context, cfg *chconfig.Config, ownerDSN string) (*sqlx.DB, error) {
	if !cfg.Storage.IsPostgres() {
		return nil, errors.New(`pgserver: [storage] backend is not "postgres"`)
	}
	if ownerDSN == "" {
		ownerDSN = cfg.Storage.DSN
	}
	db, err := postgres.Open(ctx, ownerDSN)
	if err != nil {
		return nil, fmt.Errorf("connect to Postgres: %w", err)
	}
	return db, nil
}

// NodeOpts returns the server options of a node on the Postgres database db (connected as the application role,
// migrated): base with every repository on Postgres, the tenant of each request from the resolver, the vault keys of
// [vault] key_provider, and, for [storage] role = "api", no client listener and work on clients refused as not
// connected; the cluster coordinator of [cluster] node_id (a node with a client listener takes the cluster's
// client listener lease, or is refused while another node holds it). It refuses a database that is not migrated (run rportd migrate) and a connection row-level security
// would not apply to (superuser, BYPASSRLS or table owner). release frees what NodeOpts opened (not db); call it
// after the server stopped.
func NodeOpts(ctx context.Context, cfg *chconfig.Config, db *sqlx.DB, base *chserver.ServerOpts, logw io.Writer) (_ *chserver.ServerOpts, release func(), _ error) {
	if !cfg.Storage.IsPostgres() {
		return nil, nil, errors.New(`pgserver: [storage] backend is not "postgres"`)
	}
	if err := migrate.Check(ctx, db); err != nil {
		return nil, nil, err
	}
	// row-level security is the second guard between tenants; refuse a connection it wouldn't apply to
	if err := postgres.CheckAppRole(ctx, db); err != nil {
		return nil, nil, err
	}
	if logw == nil {
		logw = io.Discard
	}

	opts := &chserver.ServerOpts{}
	if base != nil {
		*opts = *base
	}
	if opts.FilesAPI == nil {
		opts.FilesAPI = files.NewFileSystem()
	}
	// take the change feed's head before the server loads what it caches
	coord, err := NewCoordinator(ctx, db, cfg.Cluster.NodeID, logger.NewLogger("cluster", cfg.Logging.LogOutput, cfg.Logging.LogLevel))
	if err != nil {
		return nil, nil, err
	}
	if cfg.Storage.Role != chconfig.StorageRoleAPI {
		// phase 2: one node of a cluster accepts the client connections
		if err := coord.AcquireClientListener(ctx); err != nil {
			return nil, nil, err
		}
	}
	opts.Cluster = coord

	opts.Repositories = Repositories(db, cfg)
	opts.TenantResolver = NewResolver(db, cfg.API.JWTSecret)
	// every repository is injected; refuse rather than create any SQLite file
	opts.SkipMigrations = true
	if cfg.Storage.Role == chconfig.StorageRoleAPI {
		opts.DisableClientListener = true
		opts.Dispatcher = NotConnectedDispatcher{}
	}

	keys, closeFn, err := VaultKeys(ctx, cfg, db, logw)
	if err != nil {
		return nil, nil, err
	}
	opts.VaultKeys = keys
	return opts, closeFn, nil
}

// VaultKeys returns the vault key provider of [vault] key_provider on db: nil for the passphrase vault, otherwise
// the managed provider (wrapped keys stored in db). closeFn releases what it opened.
func VaultKeys(ctx context.Context, cfg *chconfig.Config, db *sqlx.DB, logw io.Writer) (_ vault.KeyProvider, closeFn func(), _ error) {
	if logw == nil {
		logw = io.Discard
	}
	switch cfg.Vault.KeyProvider {
	case "", chconfig.KeyProviderPassphrase:
		// the vault is initialized and unlocked with a passphrase, per tenant, as standalone
		return nil, func() {}, nil
	case chconfig.KeyProviderKMS:
		p, c, err := KMSVaultKeys(ctx, cfg.Vault.KMSKey, db, logw)
		if err != nil {
			return nil, nil, err
		}
		return p, c, nil
	case chconfig.KeyProviderLocal:
		k, err := vaultkms.NewLocalKMS(cfg.Vault.LocalKeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("vault: %w", err)
		}
		p, err := vaultKeys(k, localKeyName, db, logw)
		if err != nil {
			return nil, nil, err
		}
		return p, func() {}, nil
	default:
		return nil, nil, fmt.Errorf("vault: unknown key provider %q", cfg.Vault.KeyProvider)
	}
}
