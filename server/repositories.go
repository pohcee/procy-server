package chserver

import (
	"github.com/openrport/openrport/server/api/authorization"
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/api/jobs/schedule"
	"github.com/openrport/openrport/server/api/session"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/auditlog"
	"github.com/openrport/openrport/server/cgroups"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clients/storedtunnels"
	"github.com/openrport/openrport/server/clientsauth"
	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/server/monitoring"
	"github.com/openrport/openrport/server/notifications"
	"github.com/openrport/openrport/server/vault"
)

// Repositories is the storage the server runs on, one repository per data domain. Pass it in
// ServerOpts.Repositories to replace any of them: a nil field gets the SQLite implementation in the data directory,
// and the SQLite file of a field that is set is not opened. Every repository method keeps the tenant rules of its
// interface (a ctx without a tenant is refused).
//
// On shutdown the server calls Close on Jobs, ClientGroups, Notifications, Library, APISessions, the Vault's current
// provider and, only when the audit log is enabled, AuditLog, whether given or opened. It does not call Close on
// Clients, Monitoring or APITokens: it closes clients.db (and notifications.db) directly, and leaves monitoring.db and
// api_token.db to process exit.
type Repositories struct {
	// Clients stores the connected and disconnected clients (clients.db). It shares the file with StoredTunnels;
	// the file is opened when either of the two is nil.
	Clients clients.ClientStore
	// StoredTunnels stores the tunnels saved for each client (clients.db).
	StoredTunnels storedtunnels.Provider
	// ClientGroups stores the client groups (client_groups.db).
	ClientGroups cgroups.ClientGroupProvider
	// Jobs stores jobs and multi-client jobs (jobs.db). It shares the file with Schedules; the file is opened when
	// either of the two is nil.
	Jobs jobs.Provider
	// Schedules stores the scheduled jobs (jobs.db). The SQLite implementation reads the jobs table to count the
	// jobs of a schedule that are in progress and deletes a schedule's jobs with it; an injected Schedules must do
	// both itself against whatever Jobs stores.
	Schedules schedule.Provider
	// Monitoring stores client measurements (monitoring.db).
	Monitoring monitoring.DBProvider
	// Notifications stores the notification log (notifications.db). Old entries are deleted periodically only when
	// the repository also implements the cleaner's sqlite.Cleanable interface.
	Notifications notifications.Repository
	// Library stores library entries and their versions (library.db). The import of scripts and commands from old
	// versions runs only when the repository also implements library.LegacyImporter.
	Library library.Repository
	// APITokens stores the users' API tokens (api_token.db).
	APITokens authorization.DbProvider
	// APISessions stores the API sessions (api_sessions.db).
	APISessions session.StorageProvider
	// Vault gives the vault storage (vault.sqlite.db). The SQLite factory creates its file when the vault is
	// initialised and opens it at startup if it exists; an injected factory does both itself.
	Vault vault.DbProviderFactory
	// AuditLog stores the audit log (auditlog.db, rotated). It is used only when the audit log is enabled.
	AuditLog auditlog.Provider
	// Users is the API user source. nil: chosen from the config as without injection (auth file, static user or
	// the [database] user table).
	Users users.Provider
	// ClientAuth is the client credential source. nil: chosen from the config as without injection (auth, auth
	// file or the [database] auth table).
	ClientAuth clientsauth.Provider
}
