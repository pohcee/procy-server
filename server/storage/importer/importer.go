// Package importer copies the data of a stopped standalone server (its data dir of SQLite files and the users and
// client credentials its config names) into a tenant of the Postgres storage.
//
// It never writes into the data dir: files are opened read-only and immutable, or read from a private copy when a
// WAL must be replayed or an older schema migrated. A data dir a running process holds is refused. Everything is
// written in one transaction, with INSERT … ON CONFLICT DO NOTHING on each table's key, so a second run adds nothing
// and reports every row as present.
package importer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"

	apisessionsmigration "github.com/openrport/openrport/db/migration/api_sessions"
	apitokenmigration "github.com/openrport/openrport/db/migration/api_token"
	auditlogmigration "github.com/openrport/openrport/db/migration/auditlog"
	clientgroupsmigration "github.com/openrport/openrport/db/migration/client_groups"
	clientsmigration "github.com/openrport/openrport/db/migration/clients"
	jobsmigration "github.com/openrport/openrport/db/migration/jobs"
	librarymigration "github.com/openrport/openrport/db/migration/library"
	monitoringmigration "github.com/openrport/openrport/db/migration/monitoring"
	vaultsmigration "github.com/openrport/openrport/db/migration/vaults"
	notificationsmigration "github.com/openrport/openrport/server/notifications/repository/sqlite"
	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/vault"
	"github.com/openrport/openrport/share/tenant"
)

// Options configure one import.
type Options struct {
	// From is the standalone data dir. Empty: the data_dir of FromConfig.
	From string
	// FromConfig is the standalone rportd.conf, read for the sources of users and client credentials. Without it,
	// neither is imported.
	FromConfig string
	// Tenant receives the data; it must exist. Empty: the default tenant.
	Tenant tenant.ID
	// DryRun does everything in a transaction that is rolled back; the report shows what would be written.
	DryRun bool
	// WithHistory also copies jobs, multi-client jobs, the audit log, monitoring and the notification log.
	WithHistory bool
	// VaultPassphrase is the source vault's passphrase. Needed only with VaultKeys, to re-encrypt the values.
	VaultPassphrase string
	// VaultKeys is the target's managed vault key provider. Nil: the target uses passphrase vaults, and the vault
	// values and status are copied as they are (the source passphrase unlocks the target vault).
	VaultKeys vault.KeyProvider
	// Log receives progress lines. Nil: discarded.
	Log io.Writer
}

// The SQLite files of a standalone data dir (server/storage_sqlite.go).
const (
	fileClients       = "clients.db"
	fileClientGroups  = "client_groups.db"
	fileJobs          = "jobs.db"
	fileLibrary       = "library.db"
	fileAPITokens     = "api_token.db"
	fileAPISessions   = "api_sessions.db"
	fileVault         = "vault.sqlite.db"
	fileMonitoring    = "monitoring.db"
	fileNotifications = "notifications.db"
	fileAuditLog      = "auditlog.db"
)

// errDryRun rolls back the transaction of a dry run.
var errDryRun = errors.New("dry run")

type importer struct {
	o      Options
	tid    tenant.ID
	src    *source
	auth   *authSource
	counts []*EntityCount
	rep    *Report

	dbs   map[string]*sourceDB
	audit []*sourceDB // audit log files, oldest first

	creds  []clientCred
	users  []srcUser
	groups []srcGroup

	vault *vaultSource
}

// Run imports the standalone data described by o into the tenant o.Tenant of db. db must be able to write the
// tenant's tables (the application role is enough); the tenant must exist. The report counts every entity; a
// returned error means nothing was written.
func Run(ctx context.Context, db *sqlx.DB, o Options) (*Report, error) {
	if o.Log == nil {
		o.Log = io.Discard
	}
	tid := o.Tenant
	if tid == "" {
		tid = tenant.Default
	}
	im := &importer{o: o, tid: tid, rep: &Report{Tenant: tid, DryRun: o.DryRun}, dbs: map[string]*sourceDB{}}
	im.src = &source{log: im.logf}
	defer im.src.close()

	if err := im.prepare(ctx, db); err != nil {
		return nil, err
	}
	err := postgres.InTenantID(ctx, db, tid, func(tx *sqlx.Tx) error {
		w := &writer{ctx: ctx, tx: tx, tid: tid, cleaned: map[string]int{}}
		if err := im.write(w); err != nil {
			return err
		}
		tables := make([]string, 0, len(w.cleaned))
		for t := range w.cleaned {
			tables = append(tables, t)
		}
		sort.Strings(tables)
		for _, t := range tables {
			im.rep.warnf("%s: %d text values had NUL bytes or invalid UTF-8, which Postgres can't store: NULs were dropped, invalid bytes replaced", t, w.cleaned[t])
		}
		if o.DryRun {
			return errDryRun
		}
		// after the inserts, inside the transaction: no new row can take an imported id in between
		return im.advanceSequences(w)
	})
	if err != nil && !errors.Is(err, errDryRun) {
		return nil, err
	}
	for _, c := range im.counts {
		im.rep.Entities = append(im.rep.Entities, *c)
	}
	return im.rep, nil
}

func (im *importer) logf(format string, args ...any) {
	fmt.Fprintf(im.o.Log, format+"\n", args...)
}

// count returns the counter of the named entity, adding it to the report in order of first use.
func (im *importer) count(name string) *EntityCount {
	for _, c := range im.counts {
		if c.Name == name {
			return c
		}
	}
	c := &EntityCount{Name: name}
	im.counts = append(im.counts, c)
	return c
}

// prepare checks the target tenant and the source, and reads everything that must be read before writing: the
// config's auth sources and the vault.
func (im *importer) prepare(ctx context.Context, db *sqlx.DB) error {
	var status string
	err := db.GetContext(ctx, &status, "SELECT status FROM tenants WHERE id = $1", string(im.tid))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("tenant %s does not exist; create it with rportd tenant add", im.tid)
	}
	if err != nil {
		return fmt.Errorf("check tenant %s: %w", im.tid, err)
	}
	if status == "reserved" {
		return fmt.Errorf("tenant %s is reserved and can't receive data", im.tid)
	}

	var cfg *sourceConfig
	if im.o.FromConfig != "" {
		if cfg, err = loadSourceConfig(im.o.FromConfig); err != nil {
			return err
		}
	}
	dir := im.o.From
	if dir == "" && cfg != nil {
		dir = cfg.configDataDir()
	}
	if dir == "" {
		return errors.New("the source data dir is required")
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("source data dir: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("source data dir %s is not a directory", dir)
	}
	im.src.dir = dir

	var authDBPath string
	if cfg != nil {
		im.auth = &authSource{cfg: cfg, paths: pathResolver{configDataDir: cfg.configDataDir(), dataDir: dir, configDir: cfg.dir}}
		if authDBPath, err = im.auth.authDBPath(); err != nil {
			return err
		}
	}
	var extra []string
	if authDBPath != "" {
		extra = append(extra, authDBPath)
	}
	warnings, err := guard(dir, extra)
	if err != nil {
		return err
	}
	im.rep.Warnings = append(im.rep.Warnings, warnings...)

	if im.src.scratch, err = os.MkdirTemp("", "rport-import-"); err != nil {
		return err
	}
	if err := im.openSources(ctx); err != nil {
		return err
	}
	if im.auth != nil {
		if authDBPath != "" {
			if im.auth.db, err = im.src.open(authDBPath, nil); err != nil {
				return fmt.Errorf("[database] %w", err)
			}
			if im.auth.db == nil {
				return fmt.Errorf("[database] %s does not exist", authDBPath)
			}
		}
		var note string
		if im.creds, note, err = im.auth.clientCreds(); err != nil {
			return err
		}
		if note != "" {
			im.rep.warnf("%s", note)
		}
		if im.users, im.groups, note, err = im.auth.apiUsers(); err != nil {
			return err
		}
		if note != "" {
			im.rep.warnf("%s", note)
		}
	} else {
		im.rep.warnf("no source config given: users and client credentials are not imported")
	}
	return im.prepareVault(ctx)
}

// openSources opens the data dir's SQLite files at their current schema and refuses rows of other tenants.
func (im *importer) openSources(ctx context.Context) error {
	files := []struct {
		name    string
		history bool
	}{
		{fileClients, false}, {fileClientGroups, false}, {fileJobs, false}, {fileLibrary, false},
		{fileAPITokens, false}, {fileAPISessions, false}, {fileVault, false},
		{fileMonitoring, true}, {fileNotifications, true},
	}
	for _, f := range files {
		migr := migrationsOf(f.name)
		if f.history && !im.o.WithHistory {
			migr = nil // only counted: read as it is
		}
		d, err := im.src.open(filepath.Join(im.src.dir, f.name), migr)
		if err != nil {
			return err
		}
		if d == nil {
			continue
		}
		if f.name == fileLibrary {
			var n int
			if d, n, err = im.src.importLegacyLibrary(ctx, d, migr); err != nil {
				return err
			}
			if n > 0 {
				im.rep.warnf("library: %d old scripts and commands were converted into entries (as the server does on its first start after the upgrade)", n)
			}
		}
		im.dbs[f.name] = d
	}
	audit, err := filepath.Glob(filepath.Join(im.src.dir, "auditlog*.db"))
	if err != nil {
		return err
	}
	// rotated files carry their date (auditlog.2006-01-02.db) and sort before the current auditlog.db
	sort.Slice(audit, func(i, j int) bool {
		ci, cj := filepath.Base(audit[i]) == fileAuditLog, filepath.Base(audit[j]) == fileAuditLog
		if ci != cj {
			return cj
		}
		return audit[i] < audit[j]
	})
	for _, p := range audit {
		var migr = auditlogmigration.FS
		if !im.o.WithHistory {
			migr = nil
		}
		d, err := im.src.open(p, migr)
		if err != nil {
			return err
		}
		if d != nil {
			im.audit = append(im.audit, d)
		}
	}
	// history that is only counted is not checked: it is not imported
	for name, d := range im.dbs {
		if !im.o.WithHistory && (name == fileMonitoring || name == fileNotifications) {
			continue
		}
		if err := checkTenants(d); err != nil {
			return err
		}
	}
	if im.o.WithHistory {
		for _, d := range im.audit {
			if err := checkTenants(d); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkTenants runs checkTenant on every table of d.
func checkTenants(d *sourceDB) error {
	var tables []string
	if err := d.db.Select(&tables, "SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name"); err != nil {
		return err
	}
	for _, t := range tables {
		if err := checkTenant(d, t); err != nil {
			return err
		}
	}
	return nil
}

func migrationsOf(name string) fs.FS {
	switch name {
	case fileClients:
		return clientsmigration.FS
	case fileClientGroups:
		return clientgroupsmigration.FS
	case fileJobs:
		return jobsmigration.FS
	case fileLibrary:
		return librarymigration.FS
	case fileAPITokens:
		return apitokenmigration.FS
	case fileAPISessions:
		return apisessionsmigration.FS
	case fileVault:
		return vaultsmigration.FS
	case fileMonitoring:
		return monitoringmigration.FS
	case fileNotifications:
		return notificationsmigration.Migrations
	}
	return nil
}

// write copies every entity, in an order that satisfies the foreign keys.
func (im *importer) write(w *writer) error {
	steps := []func(*writer) error{
		im.writeClientAuth,
		im.writeClients,
		im.writeClientGroups,
		im.writeUsers,
		im.writeAPITokens,
		im.writeAPISessions,
		im.writeLibrary,
		im.writeSchedules,
		im.writeVault,
		im.writeHistory,
	}
	for _, step := range steps {
		if err := step(w); err != nil {
			return err
		}
	}
	return nil
}

// sequences whose values the import may preserve: after it, each must hand out ids above the imported ones.
var sequences = []struct{ table, column string }{
	{"api_sessions", "session_id"},
	{"notifications_log", "id"},
	{"vault_values", "id"},
	{"vault_status", "id"},
}

// advanceSequences moves each sequence past the largest id of its table. It never moves one back: the sequences
// are shared by all tenants. setval needs UPDATE on the sequence, which the application role doesn't have; then the
// sequence is advanced with nextval instead.
func (im *importer) advanceSequences(w *writer) error {
	for _, s := range sequences {
		var seq sql.NullString
		if err := w.tx.GetContext(w.ctx, &seq, "SELECT pg_get_serial_sequence($1, $2)", s.table, s.column); err != nil {
			return err
		}
		if !seq.Valid {
			return fmt.Errorf("%s.%s has no sequence", s.table, s.column)
		}
		var maxID sql.NullInt64
		if err := w.tx.GetContext(w.ctx, &maxID, fmt.Sprintf("SELECT max(%s) FROM %s WHERE tenant_id = $1", s.column, s.table), string(w.tid)); err != nil {
			return err
		}
		if !maxID.Valid {
			continue
		}
		var last int64
		var called bool
		if err := w.tx.QueryRowxContext(w.ctx, "SELECT last_value, is_called FROM "+seq.String).Scan(&last, &called); err != nil {
			return err
		}
		next := last
		if called {
			next++
		}
		if next > maxID.Int64 {
			continue
		}
		if _, err := w.tx.ExecContext(w.ctx, "SAVEPOINT import_setval"); err != nil {
			return err
		}
		if _, err := w.tx.ExecContext(w.ctx, "SELECT setval($1::regclass, $2)", seq.String, maxID.Int64); err == nil {
			if _, err := w.tx.ExecContext(w.ctx, "RELEASE SAVEPOINT import_setval"); err != nil {
				return err
			}
			continue
		} else if pgErr := (*pgconn.PgError)(nil); !errors.As(err, &pgErr) || pgErr.Code != "42501" { // insufficient_privilege
			return fmt.Errorf("advance %s: %w", seq.String, err)
		}
		if _, err := w.tx.ExecContext(w.ctx, "ROLLBACK TO SAVEPOINT import_setval"); err != nil {
			return err
		}
		if _, err := w.tx.ExecContext(w.ctx, "SELECT max(nextval($1::regclass)) FROM generate_series(1, $2::bigint)", seq.String, maxID.Int64-next+1); err != nil {
			return fmt.Errorf("advance %s: %w", seq.String, err)
		}
	}
	return nil
}
