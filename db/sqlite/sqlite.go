package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source"
	bindata "github.com/golang-migrate/migrate/v4/source/go_bindata"
	"github.com/jmoiron/sqlx"
	sqlite3drv "github.com/mattn/go-sqlite3"

	"github.com/openrport/openrport/share/logger"
)

const (
	WALEnabled                  = "_journal_mode=WAL"
	defaultDelayBetweenAttempts = 200 * time.Millisecond
	DefaultMaxAttempts          = 5
	DefaultMaxOpenConnections   = 1
)

type DataSourceOptions struct {
	WALEnabled         bool
	MaxOpenConnections int
}

// New opens the SQLite file at path and migrates it to the latest version
// of the given migrations (files at the root of the fs.FS). The db is closed if
// migrating fails.
func New(path string, migrations fs.FS, opts DataSourceOptions) (*sqlx.DB, error) {
	db, err := Open(path, opts)
	if err != nil {
		return nil, err
	}
	if err := Migrate(db, migrations); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Open connects to the SQLite file at path without migrating it.
func Open(path string, opts DataSourceOptions) (*sqlx.DB, error) {
	dataSourceName := path
	if opts.WALEnabled {
		dataSourceName += "?" + WALEnabled
	}
	db, err := sqlx.Connect("sqlite3", dataSourceName)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to DB: %v", err)
	}
	if path != ":memory:" {
		if err = os.Chmod(path, 0600); err != nil {
			db.Close()
			return nil, fmt.Errorf("failed to chmod %s: %s", path, err)
		}
	}

	maxConns := opts.MaxOpenConnections
	if maxConns == 0 {
		maxConns = DefaultMaxOpenConnections
	}
	db.SetMaxOpenConns(maxConns)

	return db, nil
}

// Migrate runs every migration in migrations (files at its root) that db has not applied yet.
func Migrate(db *sqlx.DB, migrations fs.FS) error {
	m, err := newMigrate(db, migrations)
	if err != nil {
		return err
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("failed to migrate DB to the latest version: %v", err)
	}
	return nil
}

// MigrateTo migrates db up or down to the given version of migrations.
func MigrateTo(db *sqlx.DB, migrations fs.FS, version uint) error {
	m, err := newMigrate(db, migrations)
	if err != nil {
		return err
	}
	if err := m.Migrate(version); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("failed to migrate DB to version %d: %v", version, err)
	}
	return nil
}

// newMigrate returns a migrate instance for db. It is not closed: closing it
// would close db.
func newMigrate(db *sqlx.DB, migrations fs.FS) (*migrate.Migrate, error) {
	sourceDriver, err := newSourceDriver(migrations)
	if err != nil {
		return nil, err
	}

	dbDriver, err := sqlite3.WithInstance(db.DB, &sqlite3.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to init DB migration driver: %v", err)
	}

	m, err := migrate.NewWithInstance("go-bindata", sourceDriver, "sqlite3", dbDriver)
	if err != nil {
		return nil, fmt.Errorf("failed to init DB migration instance: %v", err)
	}
	return m, nil
}

// CheckVersion returns an error unless db is clean and at the latest version of migrations; a db newer than
// migrations is refused too.
// It does not change db.
func CheckVersion(db *sqlx.DB, migrations fs.FS) error {
	latest, err := LatestVersion(migrations)
	if err != nil {
		return err
	}

	current, dirty := -1, false
	var tables int
	if err := db.Get(&tables, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'"); err != nil {
		return fmt.Errorf("failed to read DB version: %v", err)
	}
	if tables > 0 {
		err := db.QueryRow("SELECT version, dirty FROM schema_migrations LIMIT 1").Scan(&current, &dirty)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("failed to read DB version: %v", err)
		}
	}

	if dirty {
		return fmt.Errorf("database is dirty at version %d (a migration failed part way): fix the schema by hand, "+
			"set version and dirty in schema_migrations to match it, then run \"rportd migrate\"", current)
	}
	if current > latest {
		return fmt.Errorf("database is at version %d, newer than this rportd (%d)", current, latest)
	}
	if current < latest {
		return fmt.Errorf("database is at version %d, expected %d; run \"rportd migrate\"", current, latest)
	}
	return nil
}

// LatestVersion returns the highest migration version in migrations.
func LatestVersion(migrations fs.FS) (int, error) {
	sourceDriver, err := newSourceDriver(migrations)
	if err != nil {
		return 0, err
	}
	defer sourceDriver.Close()

	v, err := sourceDriver.First()
	if err != nil {
		return 0, fmt.Errorf("failed to read migrations: %v", err)
	}
	for {
		next, err := sourceDriver.Next(v)
		if errors.Is(err, os.ErrNotExist) {
			return int(v), nil
		}
		if err != nil {
			return 0, fmt.Errorf("failed to read migrations: %v", err)
		}
		v = next
	}
}

// newSourceDriver feeds the files at the root of migrations to golang-migrate
// through its generic name/content adapter.
func newSourceDriver(migrations fs.FS) (source.Driver, error) {
	entries, err := fs.ReadDir(migrations, ".")
	if err != nil {
		return nil, fmt.Errorf("failed to list migrations: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	s := bindata.Resource(names, func(name string) ([]byte, error) {
		return fs.ReadFile(migrations, name)
	})
	d, err := bindata.WithInstance(s)
	if err != nil {
		return nil, fmt.Errorf("failed to init DB source driver: %v", err)
	}
	return d, nil
}

func WithRetryWhenBusy[R any](retryAbleFn func() (result R, err error), label string, l *logger.Logger) (result R, err error) {
	for attempt := 1; attempt <= DefaultMaxAttempts; attempt++ {
		if attempt > 1 && err != nil {
			sqlErr, ok := err.(sqlite3drv.Error)
			if ok && sqlErr.Code == sqlite3drv.ErrBusy {
				l.Debugf("%s: attempt %d: source err = %+v\n", label, attempt, err)
				jitter := time.Duration((rand.Intn(100))) * time.Millisecond
				time.Sleep(defaultDelayBetweenAttempts + jitter)
			} else {
				// a different error from database busy, so fail immediately
				l.Debugf("%s: attempt %d: non-retryable err = %+v\n", label, attempt, err)
				return result, err
			}
		}
		// make an attempt to complete the retryable fn
		result, err = retryAbleFn()
		// if no error then return immediately if with success result
		if err == nil {
			return result, nil
		}
	}

	l.Errorf("%s: failed after max attempts: err = %+v\n", label, err)
	return result, err
}
