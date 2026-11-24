// Package migration lists the embedded SQL migration sets of every rportd SQLite database.
package migration

import (
	"io/fs"

	"github.com/openrport/openrport/db/migration/api_sessions"
	"github.com/openrport/openrport/db/migration/api_token"
	"github.com/openrport/openrport/db/migration/auditlog"
	"github.com/openrport/openrport/db/migration/client_groups"
	"github.com/openrport/openrport/db/migration/clients"
	"github.com/openrport/openrport/db/migration/jobs"
	"github.com/openrport/openrport/db/migration/library"
	"github.com/openrport/openrport/db/migration/monitoring"
	"github.com/openrport/openrport/db/migration/vaults"
	notifications "github.com/openrport/openrport/server/notifications/repository/sqlite"
)

// Set is the migrations of one database, named after the database.
type Set struct {
	Name string
	FS   fs.FS
}

// All returns every migration set used by rportd.
func All() []Set {
	return []Set{
		{Name: "clients", FS: clients.FS},
		{Name: "jobs", FS: jobs.FS},
		{Name: "client_groups", FS: client_groups.FS},
		{Name: "monitoring", FS: monitoring.FS},
		{Name: "notifications", FS: notifications.Migrations},
		{Name: "library", FS: library.FS},
		{Name: "api_token", FS: api_token.FS},
		{Name: "api_sessions", FS: api_sessions.FS},
		{Name: "vaults", FS: vaults.FS},
		{Name: "auditlog", FS: auditlog.FS},
	}
}
