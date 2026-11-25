package chserver

// All opening and migrating of rportd's SQLite files happens here; the rest of the server reaches storage through
// Repositories only.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/patrickmn/go-cache"

	// sql drivers
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/mattn/go-sqlite3"

	apisessionsmigration "github.com/openrport/openrport/db/migration/api_sessions"
	apitokenmigration "github.com/openrport/openrport/db/migration/api_token"
	auditlogmigration "github.com/openrport/openrport/db/migration/auditlog"
	clientgroupsmigration "github.com/openrport/openrport/db/migration/client_groups"
	clientsmigration "github.com/openrport/openrport/db/migration/clients"
	jobsmigration "github.com/openrport/openrport/db/migration/jobs"
	librarymigration "github.com/openrport/openrport/db/migration/library"
	monitoringmigration "github.com/openrport/openrport/db/migration/monitoring"
	vaultsmigration "github.com/openrport/openrport/db/migration/vaults"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/api/authorization"
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/api/jobs/schedule"
	"github.com/openrport/openrport/server/api/session"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/auditlog"
	"github.com/openrport/openrport/server/cgroups"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clients/storedtunnels"
	"github.com/openrport/openrport/server/clientsauth"
	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/server/monitoring"
	notificationsSQLite "github.com/openrport/openrport/server/notifications/repository/sqlite"
	"github.com/openrport/openrport/server/oidc"
	"github.com/openrport/openrport/server/vault"
	"github.com/openrport/openrport/share/logger"
)

// auditLogDBName is the audit log file the rotation provider writes to (rotated files get a date in their name).
const auditLogDBName = "auditlog.db"

// sqliteFile is one SQLite database in the data directory.
type sqliteFile struct {
	path       string
	migrations fs.FS
	opts       sqlite.DataSourceOptions
}

// sqliteFileSet is every SQLite database rportd may keep in its data directory.
type sqliteFileSet struct {
	clients, jobs, clientGroups, monitoring, notifications, library, apiToken, apiSessions, auditLog, vault sqliteFile
}

func newSQLiteFileSet(config *chconfig.Config) sqliteFileSet {
	dir := config.Server.DataDir
	opts := config.Server.GetSQLiteDataSourceOptions()
	file := func(name string, migrations fs.FS) sqliteFile {
		return sqliteFile{path: filepath.Join(dir, name), migrations: migrations, opts: opts}
	}

	// particularly the client.db needs performant db access, so allow multi-threaded access
	// and use the RetryWhenBusy fn to ensure writes succeed if we get a busy error due to
	// concurrent thread access.
	clientsOpts := opts
	clientsOpts.MaxOpenConnections = DefaultMaxClientDBConnections

	return sqliteFileSet{
		clients:       sqliteFile{path: filepath.Join(dir, "clients.db"), migrations: clientsmigration.FS, opts: clientsOpts},
		jobs:          file("jobs.db", jobsmigration.FS),
		clientGroups:  file("client_groups.db", clientgroupsmigration.FS),
		monitoring:    file("monitoring.db", monitoringmigration.FS),
		notifications: file("notifications.db", notificationsSQLite.Migrations),
		library:       file("library.db", librarymigration.FS),
		apiToken:      file("api_token.db", apitokenmigration.FS),
		apiSessions:   file("api_sessions.db", apisessionsmigration.FS),
		auditLog:      file(auditLogDBName, auditlogmigration.FS),
		vault:         sqliteFile{path: config.GetVaultDBPath(), migrations: vaultsmigration.FS, opts: vault.DataSourceOptions},
	}
}

// sqliteDatabases returns the files the server opens at startup with this config, in opening order: every fixed
// database, the audit log when it is enabled and the vault when it has been initialized (its file exists).
func sqliteDatabases(config *chconfig.Config) ([]sqliteFile, error) {
	set := newSQLiteFileSet(config)
	files := []sqliteFile{
		set.clients, set.jobs, set.clientGroups, set.monitoring, set.notifications, set.library, set.apiToken,
		set.apiSessions,
	}
	if config.API.AuditLog.Enable {
		files = append(files, set.auditLog)
	}
	vaultExists, err := fileExists(set.vault.path)
	if err != nil {
		return nil, err
	}
	if vaultExists {
		files = append(files, set.vault)
	}
	return files, nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("failed to check if %q exists: %w", path, err)
}

// MigrateSQLite migrates every SQLite database the server would open with config to the latest version and closes
// it again: the fixed databases, the audit log when it is enabled and the vault only if it has been initialized. It
// creates the data directory and missing databases. It logs one line per file with the version it is at.
func MigrateSQLite(config *chconfig.Config, l *logger.Logger) error {
	if config.Server.DataDir == "" {
		return errors.New("data directory cannot be empty")
	}
	if err := os.MkdirAll(config.Server.DataDir, os.ModePerm); err != nil {
		return fmt.Errorf("failed to create data dir %q: %w", config.Server.DataDir, err)
	}
	files, err := sqliteDatabases(config)
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := migrateSQLiteFile(f, l); err != nil {
			return err
		}
	}
	return nil
}

func migrateSQLiteFile(f sqliteFile, l *logger.Logger) error {
	db, err := sqlite.Open(f.path, f.opts)
	if err != nil {
		return fmt.Errorf("%s: %w", f.path, err)
	}
	defer db.Close()

	before, err := schemaVersion(db)
	if err != nil {
		return fmt.Errorf("%s: %w", f.path, err)
	}
	if err := sqlite.Migrate(db, f.migrations); err != nil {
		return fmt.Errorf("%s: %w", f.path, err)
	}
	after, err := schemaVersion(db)
	if err != nil {
		return fmt.Errorf("%s: %w", f.path, err)
	}

	if before == after {
		l.Infof("%s: at version %d, nothing to migrate", f.path, after)
	} else {
		l.Infof("%s: migrated to version %d", f.path, after)
	}
	return db.Close()
}

// schemaVersion returns the migration version db is at, or -1 if it has none.
func schemaVersion(db *sqlx.DB) (int, error) {
	var tables int
	if err := db.Get(&tables, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'"); err != nil {
		return 0, fmt.Errorf("failed to read DB version: %w", err)
	}
	if tables == 0 {
		return -1, nil
	}
	var versions []int
	if err := db.Select(&versions, "SELECT version FROM schema_migrations LIMIT 1"); err != nil {
		return 0, fmt.Errorf("failed to read DB version: %w", err)
	}
	if len(versions) == 0 {
		return -1, nil
	}
	return versions[0], nil
}

// openSQLiteFile opens f and migrates it to the latest version. With skipMigrations it only opens an existing file
// and fails unless the file is at the latest version.
func openSQLiteFile(f sqliteFile, skipMigrations bool) (*sqlx.DB, error) {
	if !skipMigrations {
		db, err := sqlite.New(f.path, f.migrations, f.opts)
		if err != nil {
			return nil, fmt.Errorf("failed to open %s: %w", f.path, err)
		}
		return db, nil
	}

	exists, err := fileExists(f.path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%s: database does not exist; run \"rportd migrate\"", f.path)
	}
	db, err := sqlite.Open(f.path, f.opts)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", f.path, err)
	}
	if err := sqlite.CheckVersion(db, f.migrations); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", f.path, err)
	}
	return db, nil
}

// sqliteStorage is what openSQLiteRepositories opened.
type sqliteStorage struct {
	// opened is every file and provider opened, closed by closeAll.
	opened []io.Closer
	// shutdown closes the files no repository Close closes; the server runs them on shutdown.
	shutdown []func() error
}

// closeAll closes everything that was opened, newest first. It is for startup failures and tests; a running server
// closes its repositories and then runs shutdown.
func (s *sqliteStorage) closeAll() error {
	var errs []error
	for i := len(s.opened) - 1; i >= 0; i-- {
		errs = append(errs, s.opened[i].Close())
	}
	return errors.Join(errs...)
}

// openSQLiteRepositories returns given with every nil field set to its SQLite implementation in the data directory
// (given may be nil). The SQLite file of a domain whose repositories are all given is not opened. Files are migrated
// to the latest version unless skipMigrations, in which case they must already be at it. Users and ClientAuth, when
// nil, are chosen from the config; the [database] connection is made only for them.
func openSQLiteRepositories(ctx context.Context, config *chconfig.Config, given *Repositories, skipMigrations bool, l *logger.Logger) (_ *Repositories, _ *sqliteStorage, err error) {
	r := Repositories{}
	if given != nil {
		r = *given
	}
	st := &sqliteStorage{}
	defer func() {
		if err != nil {
			_ = st.closeAll()
		}
	}()

	set := newSQLiteFileSet(config)
	open := func(f sqliteFile) (*sqlx.DB, error) {
		db, err := openSQLiteFile(f, skipMigrations)
		if err != nil {
			return nil, err
		}
		st.opened = append(st.opened, db)
		return db, nil
	}

	if r.Clients == nil || r.StoredTunnels == nil {
		db, err := open(set.clients)
		if err != nil {
			return nil, nil, err
		}
		if r.Clients == nil {
			r.Clients = clients.NewSQLiteStore(db, keepDisconnectedClients(config))
		}
		if r.StoredTunnels == nil {
			r.StoredTunnels = storedtunnels.NewSQLiteProvider(db)
		}
		st.shutdown = append(st.shutdown, db.Close)
	}

	if r.Jobs == nil || r.Schedules == nil {
		db, err := open(set.jobs)
		if err != nil {
			return nil, nil, err
		}
		if r.Jobs == nil {
			// closing the jobs provider closes jobs.db
			r.Jobs = jobs.NewSqliteProvider(db, l)
		} else {
			st.shutdown = append(st.shutdown, db.Close)
		}
		if r.Schedules == nil {
			r.Schedules = schedule.NewSQLiteProvider(db)
		}
	}

	if r.ClientGroups == nil {
		db, err := open(set.clientGroups)
		if err != nil {
			return nil, nil, err
		}
		r.ClientGroups, err = cgroups.NewSqliteProvider(db)
		if err != nil {
			return nil, nil, err
		}
	}

	if r.Monitoring == nil {
		db, err := open(set.monitoring)
		if err != nil {
			return nil, nil, err
		}
		l.Infof("initialized database at %s", set.monitoring.path)
		r.Monitoring = monitoring.NewSqliteProvider(db, l)
	}

	if r.Notifications == nil {
		db, err := open(set.notifications)
		if err != nil {
			return nil, nil, err
		}
		r.Notifications = notificationsSQLite.NewRepository(db, l)
		// the repository's Close only stops its streams
		st.shutdown = append(st.shutdown, db.Close)
	}

	if r.Library == nil {
		db, err := open(set.library)
		if err != nil {
			return nil, nil, err
		}
		r.Library = library.NewSQLiteStore(db)
	}

	if r.APITokens == nil {
		db, err := open(set.apiToken)
		if err != nil {
			return nil, nil, err
		}
		r.APITokens = authorization.NewSqliteProvider(db)
	}

	if r.APISessions == nil {
		db, err := open(set.apiSessions)
		if err != nil {
			return nil, nil, err
		}
		r.APISessions = session.NewSqliteProvider(db)
		r.OIDC = sqliteOIDCStore(r.OIDC, db)
	}

	if r.Vault == nil {
		r.Vault, err = openSQLiteVault(config, set.vault, skipMigrations)
		if err != nil {
			return nil, nil, err
		}
		st.opened = append(st.opened, vaultCloser{r.Vault})
	}

	if r.AuditLog == nil && config.API.AuditLog.Enable {
		r.AuditLog, err = openSQLiteAuditLog(ctx, config, set.auditLog, skipMigrations)
		if err != nil {
			return nil, nil, err
		}
		st.opened = append(st.opened, r.AuditLog)
	}

	if r.Users == nil || r.ClientAuth == nil {
		var authDB *sqlx.DB
		if config.Database.Driver != "" {
			authDB, err = sqlx.Connect(config.Database.Driver, config.Database.Dsn)
			if err != nil {
				return nil, nil, err
			}
			st.opened = append(st.opened, authDB)
			st.shutdown = append(st.shutdown, authDB.Close)
			l.Infof("DB: successfully connected to %s", config.Database.DsnForLogs())
		}

		if r.ClientAuth == nil {
			r.ClientAuth, err = getClientProvider(config, authDB)
			if err != nil {
				return nil, nil, err
			}
		}
		if r.Users == nil {
			r.Users, err = users.NewProviderFromConfig(ctx, authDB, config)
			if err != nil {
				return nil, nil, fmt.Errorf("failed init api users service: %w", err)
			}
		}
	}

	return &r, st, nil
}

// openSQLiteVault returns the vault factory on f. The file is created and migrated when the vault is initialized; if
// it already exists it is opened now.
func openSQLiteVault(config *chconfig.Config, f sqliteFile, skipMigrations bool) (vault.DbProviderFactory, error) {
	vaultLogger := logger.NewLogger("vault", config.Logging.LogOutput, config.Logging.LogLevel)
	factory := vault.NewStatefulDbProviderFactory(
		func() (vault.DbProvider, error) {
			// a file created by vault init is always migrated; an existing one is only checked with skipMigrations
			exists, err := fileExists(f.path)
			if err != nil {
				return nil, err
			}
			db, err := openSQLiteFile(f, skipMigrations && exists)
			if err != nil {
				return nil, fmt.Errorf("failed init vault DB instance: %w", err)
			}
			vaultLogger.Infof("initialized database at %s", f.path)
			return vault.NewSqliteProvider(db, vaultLogger), nil
		},
		&vault.NotInitDbProvider{},
	)

	// init vault DB if it already exists
	exists, err := fileExists(f.path)
	if err != nil {
		return nil, err
	}
	if exists {
		if err := factory.Init(); err != nil {
			return nil, err
		}
	}
	return factory, nil
}

// vaultCloser closes the vault's current provider.
type vaultCloser struct {
	f vault.DbProviderFactory
}

func (c vaultCloser) Close() error {
	return c.f.GetDbProvider().Close()
}

// openSQLiteAuditLog opens the rotating audit log. With skipMigrations auditlog.db must exist and be at the latest
// version; otherwise it is created and migrated as needed. A file a later rotation creates is always migrated.
func openSQLiteAuditLog(ctx context.Context, config *chconfig.Config, f sqliteFile, skipMigrations bool) (auditlog.Provider, error) {
	if skipMigrations {
		// fails unless the file exists and is at the latest version
		db, err := openSQLiteFile(f, true)
		if err != nil {
			return nil, err
		}
		if err := db.Close(); err != nil {
			return nil, err
		}
	}
	auditLogLogger := logger.NewLogger("auditlog", config.Logging.LogOutput, config.Logging.LogLevel)
	p, err := auditlog.NewRotationProvider(ctx, auditLogLogger, config.API.AuditLog.RotationPeriod(), config.Server.DataDir, f.opts)
	if err != nil {
		// not p: a nil *RotationProvider in the interface would not be nil
		return nil, err
	}
	return p, nil
}

func getClientProvider(config *chconfig.Config, db *sqlx.DB) (clientsauth.Provider, error) {
	if config.Server.AuthTable != "" {
		return clientsauth.NewDatabaseProvider(db, config.Server.AuthTable), nil
	}

	if config.Server.AuthFile != "" {
		return clientsauth.NewFileProvider(config.Server.AuthFile, cache.New(60*time.Minute, 15*time.Minute)), nil
	}

	if config.Server.Auth != "" {
		return clientsauth.NewSingleProvider(config.Server.AuthID, config.Server.AuthPassword), nil
	}

	return nil, errors.New("client authentication must to be enabled: set either 'auth' or 'auth_file'")
}

// sqliteOIDCStore is the OIDC store of the api_sessions database db, unless one was given.
func sqliteOIDCStore(given oidc.Store, db *sqlx.DB) oidc.Store {
	if given != nil {
		return given
	}
	return oidc.NewSQLiteStore(db)
}
