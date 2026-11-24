package pgserver

import (
	"context"
	"errors"

	"github.com/jmoiron/sqlx"

	chserver "github.com/openrport/openrport/server"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/migrate"
	pgvault "github.com/openrport/openrport/server/storage/postgres/vault"
	"github.com/openrport/openrport/server/vault"
	"github.com/openrport/openrport/share/files"
)

// Options are the optional parts of a server on Postgres.
type Options struct {
	// VaultKeys gives each tenant's vault key (vaultkms.Provider in production). nil: the vault reports "not
	// available", since a passphrase vault can't work across server instances and restarts.
	VaultKeys vault.KeyProvider
}

// NewServer builds a server on db (the application role's connection): the open-source API and web UI on
// Postgres repositories, the tenant of each request from Resolver, no client listener, and NotConnectedDispatcher for
// work on clients. db must be migrated (rportd migrate). Run it with (*chserver.Server).Run.
func NewServer(ctx context.Context, cfg *chconfig.Config, db *sqlx.DB, o Options) (*chserver.Server, error) {
	if cfg.API.Address == "" {
		return nil, errors.New("pgserver: the API address is required")
	}
	if err := migrate.Check(ctx, db); err != nil {
		return nil, err
	}
	// row-level security is the second guard between tenants; refuse a connection it wouldn't apply to
	if err := postgres.CheckAppRole(ctx, db); err != nil {
		return nil, err
	}
	repos := Repositories(db, cfg)
	if o.VaultKeys != nil {
		repos.Vault = pgvault.NewFactory(pgvault.New(db))
	}
	return chserver.NewServer(ctx, cfg, &chserver.ServerOpts{
		FilesAPI:              files.NewFileSystem(),
		Repositories:          repos,
		VaultKeys:             o.VaultKeys,
		Dispatcher:            NotConnectedDispatcher{},
		TenantResolver:        NewResolver(db, cfg.API.JWTSecret),
		DisableClientListener: true,
		// every repository is injected; refuse rather than create any SQLite file
		SkipMigrations: true,
	})
}
