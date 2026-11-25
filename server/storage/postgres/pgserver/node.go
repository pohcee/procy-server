package pgserver

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/jmoiron/sqlx"

	chserver "github.com/openrport/openrport/server"
	"github.com/openrport/openrport/server/billing"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/storage/postgres"
	pgbilling "github.com/openrport/openrport/server/storage/postgres/billing"
	"github.com/openrport/openrport/server/storage/postgres/migrate"
	pgusage "github.com/openrport/openrport/server/storage/postgres/usage"
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
// [vault] key_provider, the operator API, devices of every tenant connecting (DeviceTenants), and, for [storage]
// role = "api", no client listener and work on clients refused as not connected; for role "device" the device
// path on the device role (see deviceNodeOpts), refusing a login with more privilege than that role; the cluster
// coordinator of [cluster] node_id (a node with a client listener takes the cluster's
// client listener lease, or is refused while another node holds it). It refuses a database that is not migrated (run rportd migrate) and a connection row-level security
// would not apply to (superuser, BYPASSRLS or table owner). release frees what NodeOpts opened (not db); call it
// after the server stopped.
func NodeOpts(ctx context.Context, cfg *chconfig.Config, db *sqlx.DB, base *chserver.ServerOpts, logw io.Writer) (_ *chserver.ServerOpts, release func(), _ error) {
	if !cfg.Storage.IsPostgres() {
		return nil, nil, errors.New(`pgserver: [storage] backend is not "postgres"`)
	}
	if cfg.Storage.Role == chconfig.StorageRoleDevice {
		opts := &chserver.ServerOpts{}
		if base != nil {
			*opts = *base
		}
		if opts.FilesAPI == nil {
			opts.FilesAPI = files.NewFileSystem()
		}
		release, err := deviceNodeOpts(ctx, cfg, db, opts)
		if err != nil {
			return nil, nil, err
		}
		return opts, release, nil
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
	peerCalls := cfg.Cluster.PeerEnabled()
	switch {
	case peerCalls:
		// every client is reached through the session registry and peer calls: any node starts scheduled runs,
		// and several nodes accept client connections
	case cfg.Storage.Role == chconfig.StorageRoleAPI:
		// work on clients is refused here; the node with the client listener starts the scheduled runs
		coord.LeaveScheduledRuns()
	default:
		// without peer calls, one node of a cluster accepts the client connections
		if err := coord.AcquireClientListener(ctx); err != nil {
			return nil, nil, err
		}
	}
	opts.Cluster = coord

	opts.Repositories = Repositories(db, cfg)
	if cfg.Storage.Role == chconfig.StorageRoleAPI {
		// the clients are connected to another node: loading them must not mark them disconnected
		opts.Repositories.Clients = clients.KeepConnectionStateOnLoad(opts.Repositories.Clients)
	}
	if cfg.Metering.Enabled {
		opts.Metering = &chserver.MeteringOpts{
			Writer: pgusage.Writer{DB: db, Log: logger.NewLogger("metering", cfg.Logging.LogOutput, cfg.Logging.LogLevel)},
			Reader: pgusage.Store{DB: db},
		}
		coord.KeepUsage(pgusage.Retention{Hourly: cfg.Metering.HourlyRetention, Daily: cfg.Metering.DailyRetention, Monitoring: true})
	}
	if cfg.Billing.Enabled {
		if opts.Billing, err = BillingOpts(cfg, db); err != nil {
			coord.Release()
			return nil, nil, err
		}
		if opts.Billing.Provider, err = BillingProvider(cfg, db, opts.Billing.Plans, NewOperator(db, cfg)); err != nil {
			coord.Release()
			return nil, nil, err
		}
	}
	opts.TenantResolver = NewResolver(db, cfg.API.JWTSecret)
	opts.Operator = NewOperator(db, cfg)
	opts.Memberships = NewMemberships(db, cfg)
	// every repository is injected; refuse rather than create any SQLite file
	opts.SkipMigrations = true
	if cfg.Storage.Role == chconfig.StorageRoleAPI {
		opts.DisableClientListener = true
		opts.Dispatcher = NotConnectedDispatcher{}
	} else {
		// devices of every tenant connect; their client auth id names the tenant
		opts.DeviceTenants = NewDeviceTenants(db, keepDisconnectedClients(cfg))
	}
	// the session registry, and with peer calls the cluster dispatcher (replacing the one above) and peer listener
	cp, err := newClusterProcess(cfg, db, coord, opts)
	if err != nil {
		coord.Release()
		return nil, nil, err
	}
	opts.Peer = cp

	keys, closeFn, err := VaultKeys(ctx, cfg, db, logw)
	if err != nil {
		coord.Release()
		return nil, nil, err
	}
	opts.VaultKeys = keys
	if cfg.Signup.Enabled {
		if opts.Signup, err = NewSignup(cfg, db, opts.Operator, keys, opts.Billing); err != nil {
			coord.Release()
			closeFn()
			return nil, nil, err
		}
	}
	return opts, func() {
		// the server's run released them already, unless it never ran
		coord.Release()
		closeFn()
	}, nil
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

// BillingOpts returns the billing of an api (or all) node with [billing] enabled on db: the plans file, the
// organizations' and operators' billing, the transfer policies and the upkeep.
func BillingOpts(cfg *chconfig.Config, db *sqlx.DB) (*chserver.BillingOpts, error) {
	plans, err := billing.LoadPlans(cfg.Billing.PlansFile)
	if err != nil {
		return nil, err
	}
	store := pgbilling.Store{DB: db, Plans: plans}
	return &chserver.BillingOpts{
		Plans:    plans,
		Policies: store,
		Org:      store,
		Operator: &pgbilling.Operator{DB: db, Plans: plans},
		Upkeep: &pgbilling.Upkeep{
			DB: db, Plans: plans,
			Log:              logger.NewLogger("billing", cfg.Logging.LogOutput, cfg.Logging.LogLevel),
			ReviewAboveCents: billing.Cents(cfg.Billing.OverageReviewAbove),
			Enforce:          billing.EnforceOptions{ImplausibleFactor: cfg.Billing.ImplausibleGrowthFactor},
		},
	}, nil
}
