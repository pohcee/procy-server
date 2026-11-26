package importer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3" // the SQLite driver rportd uses

	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/server/library"
	"github.com/pohcee/procy-server/share/tenant"
)

// sourceDB is one SQLite file of the standalone server, opened without ever writing to it.
type sourceDB struct {
	name string // file name, for messages
	path string // the original file
	db   *sqlx.DB
	copy bool // db is a private copy in the scratch dir
}

// source is the standalone data dir being imported.
type source struct {
	dir     string
	scratch string
	dbs     []*sourceDB
	log     func(format string, args ...any)
}

func (s *source) close() {
	for _, d := range s.dbs {
		_ = d.db.Close()
	}
	if s.scratch != "" {
		_ = os.RemoveAll(s.scratch)
	}
}

// open opens a SQLite file of the source at the latest schema of migrations (nil: any schema). It opens the file in
// place, read-only and immutable (SQLite takes no locks and creates no side files), when that shows the current data
// at the latest schema; otherwise it reads a private copy: with a pending WAL or hot journal, which only a writable
// connection replays, or an older schema, which is migrated in the copy. It returns nil when the file doesn't exist.
func (s *source) open(path string, migrations fs.FS) (*sourceDB, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	name := filepath.Base(path)
	if !needsCopy(path) {
		db, err := openImmutable(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if migrations == nil || sqlite.CheckVersion(db, migrations) == nil {
			d := &sourceDB{name: name, path: path, db: db}
			s.dbs = append(s.dbs, d)
			s.log("read %s in place (read-only)", name)
			return d, nil
		}
		_ = db.Close()
	}
	return s.openCopy(path, migrations)
}

// openCopy copies the file (with its WAL or journal) into the scratch dir, opens the copy read-write, which replays
// the WAL into it, and migrates it to the latest schema of migrations.
func (s *source) openCopy(path string, migrations fs.FS) (*sourceDB, error) {
	name := filepath.Base(path)
	dir, err := os.MkdirTemp(s.scratch, "db-")
	if err != nil {
		return nil, err
	}
	dst := filepath.Join(dir, name)
	// the -shm index is not copied: the first connection rebuilds it
	for _, suffix := range []string{"", "-wal", "-journal"} {
		if err := copyFile(path+suffix, dst+suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("copy %s%s: %w", name, suffix, err)
		}
	}
	db, err := sqlite.Open(dst, sqlite.DataSourceOptions{})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := readable(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if migrations != nil {
		if err := sqlite.Migrate(db, migrations); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: migrating a copy to the current schema: %w", name, err)
		}
	}
	d := &sourceDB{name: name, path: path, db: db, copy: true}
	s.dbs = append(s.dbs, d)
	s.log("read %s from a copy (WAL replayed, schema migrated)", name)
	return d, nil
}

// writable returns d itself if it is a copy, otherwise a fresh copy of its file (d is closed).
func (s *source) writable(d *sourceDB, migrations fs.FS) (*sourceDB, error) {
	if d.copy {
		return d, nil
	}
	for i, o := range s.dbs {
		if o == d {
			s.dbs = append(s.dbs[:i], s.dbs[i+1:]...)
			break
		}
	}
	_ = d.db.Close()
	return s.openCopy(d.path, migrations)
}

// needsCopy reports whether a database has a pending WAL or a hot journal.
func needsCopy(path string) bool {
	if st, err := os.Stat(path + "-wal"); err == nil && st.Size() > 0 {
		return true
	}
	_, err := os.Stat(path + "-journal")
	return err == nil
}

func openImmutable(path string) (*sqlx.DB, error) {
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?mode=ro&immutable=1"
	db, err := sqlx.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := readable(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// readable forces a real read, so a file that is not a database fails here.
func readable(db *sqlx.DB) error {
	var n int
	return db.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&n)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func hasTable(db *sqlx.DB, table string) (bool, error) {
	var n int
	err := db.Get(&n, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table)
	return n > 0, err
}

func columnsOf(db *sqlx.DB, table string) (map[string]bool, error) {
	rows, err := db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols[c] = true
	}
	return cols, rows.Err()
}

func countRows(db *sqlx.DB, table string) (int, error) {
	var n int
	err := db.Get(&n, "SELECT count(*) FROM "+quoteIdent(table))
	return n, err
}

// checkTenant refuses a table holding rows of a tenant other than the default one: a standalone server has only that
// one, so anything else is not a standalone data dir.
func checkTenant(d *sourceDB, table string) error {
	cols, err := columnsOf(d.db, table)
	if err != nil {
		return err
	}
	if !cols["tenant_id"] {
		return nil
	}
	var others []string
	if err := d.db.Select(&others, "SELECT DISTINCT tenant_id FROM "+quoteIdent(table)+" WHERE tenant_id <> ? ORDER BY 1", string(tenant.Default)); err != nil {
		return fmt.Errorf("%s %s: %w", d.name, table, err)
	}
	if len(others) > 0 {
		return fmt.Errorf("%s %s has rows of tenant(s) %q: a standalone data dir has only the %q tenant", d.name, table, others, tenant.Default)
	}
	return nil
}

// importLegacyLibrary converts the scripts and commands tables of an old library.db into entries, the way rportd
// does on its first start after the upgrade, in a private copy. It returns d or its copy.
func (s *source) importLegacyLibrary(ctx context.Context, d *sourceDB, migrations fs.FS) (*sourceDB, int, error) {
	pending, err := legacyLibraryPending(d.db)
	if err != nil || !pending {
		return d, 0, err
	}
	w, err := s.writable(d, migrations)
	if err != nil {
		return nil, 0, err
	}
	n, err := library.NewSQLiteStore(w.db).ImportLegacy(tenant.WithID(ctx, tenant.Default))
	if err != nil {
		return nil, 0, fmt.Errorf("%s: converting the old scripts and commands: %w", d.name, err)
	}
	return w, n, nil
}

// legacyLibraryPending reports whether library.db has old scripts or commands not converted into entries yet.
func legacyLibraryPending(db *sqlx.DB) (bool, error) {
	var done int
	err := db.Get(&done, "SELECT count(*) FROM library_meta WHERE `key` = 'legacy_import'")
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if done > 0 {
		return false, nil
	}
	for _, t := range []string{"scripts", "commands"} {
		ok, err := hasTable(db, t)
		if err != nil {
			return false, err
		}
		if !ok {
			continue
		}
		n, err := countRows(db, t)
		if err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}

func quoteIdent(s string) string { return "`" + s + "`" }
