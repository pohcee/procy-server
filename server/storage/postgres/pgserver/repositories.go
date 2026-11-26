package pgserver

import (
	"time"

	"github.com/jmoiron/sqlx"

	chserver "github.com/pohcee/procy-server/server"
	"github.com/pohcee/procy-server/server/chconfig"
	"github.com/pohcee/procy-server/server/storage/postgres/apitokens"
	pgauditlog "github.com/pohcee/procy-server/server/storage/postgres/auditlog"
	"github.com/pohcee/procy-server/server/storage/postgres/clientauth"
	"github.com/pohcee/procy-server/server/storage/postgres/clientgroups"
	pgclients "github.com/pohcee/procy-server/server/storage/postgres/clients"
	pgjobs "github.com/pohcee/procy-server/server/storage/postgres/jobs"
	pglibrary "github.com/pohcee/procy-server/server/storage/postgres/library"
	pgmonitoring "github.com/pohcee/procy-server/server/storage/postgres/monitoring"
	pgnotifications "github.com/pohcee/procy-server/server/storage/postgres/notifications"
	pgoidc "github.com/pohcee/procy-server/server/storage/postgres/oidc"
	"github.com/pohcee/procy-server/server/storage/postgres/sessions"
	pgstoredtunnels "github.com/pohcee/procy-server/server/storage/postgres/storedtunnels"
	pgusers "github.com/pohcee/procy-server/server/storage/postgres/users"
	pgvault "github.com/pohcee/procy-server/server/storage/postgres/vault"
)

// Repositories returns every repository of the server on the Postgres database db, so the server opens no SQLite
// file. The vault storage serves both the passphrase vault and a managed key provider (ServerOpts.VaultKeys).
func Repositories(db *sqlx.DB, cfg *chconfig.Config) *chserver.Repositories {
	return &chserver.Repositories{
		Clients:       pgclients.New(db, keepDisconnectedClients(cfg)),
		StoredTunnels: pgstoredtunnels.New(db),
		ClientGroups:  clientgroups.New(db),
		Jobs:          pgjobs.New(db),
		Schedules:     pgjobs.NewScheduleStore(db),
		Monitoring:    pgmonitoring.New(db),
		Notifications: pgnotifications.New(db),
		Library:       pglibrary.New(db),
		APITokens:     apitokens.New(db),
		APISessions:   sessions.New(db),
		Vault:         pgvault.NewFactory(pgvault.New(db)),
		AuditLog:      pgauditlog.New(db),
		Users:         pgusers.New(db),
		ClientAuth:    clientauth.New(db),
		OIDC:          pgoidc.New(db),
	}
}

// keepDisconnectedClients mirrors the server's rule: disconnected clients are kept for the configured duration when
// purging is enabled, and forever otherwise.
func keepDisconnectedClients(cfg *chconfig.Config) *time.Duration {
	if cfg.Server.PurgeDisconnectedClients {
		d := cfg.Server.KeepDisconnectedClients
		return &d
	}
	return nil
}
