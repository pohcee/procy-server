package pgserver

import (
	"time"

	"github.com/jmoiron/sqlx"

	chserver "github.com/openrport/openrport/server"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/storage/postgres/apitokens"
	pgauditlog "github.com/openrport/openrport/server/storage/postgres/auditlog"
	"github.com/openrport/openrport/server/storage/postgres/clientauth"
	"github.com/openrport/openrport/server/storage/postgres/clientgroups"
	pgclients "github.com/openrport/openrport/server/storage/postgres/clients"
	pgjobs "github.com/openrport/openrport/server/storage/postgres/jobs"
	pglibrary "github.com/openrport/openrport/server/storage/postgres/library"
	pgmonitoring "github.com/openrport/openrport/server/storage/postgres/monitoring"
	pgnotifications "github.com/openrport/openrport/server/storage/postgres/notifications"
	"github.com/openrport/openrport/server/storage/postgres/sessions"
	pgstoredtunnels "github.com/openrport/openrport/server/storage/postgres/storedtunnels"
	pgusers "github.com/openrport/openrport/server/storage/postgres/users"
)

// Repositories returns every repository of the server on the Postgres database db, so the server opens no SQLite
// file. The vault is unavailableVault; NewServer replaces it with its Postgres storage when a managed key provider
// is given.
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
		Vault:         unavailableVault{},
		AuditLog:      pgauditlog.New(db),
		Users:         pgusers.New(db),
		ClientAuth:    clientauth.New(db),
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
