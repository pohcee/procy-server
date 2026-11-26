package importer

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/server/storage/postgres/pgtest"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/tenant"
)

func TestMain(m *testing.M) {
	// the SQLite stores return times in the local zone, the Postgres stores in UTC: make them one
	time.Local = time.UTC
	pgtest.Main(m)
}

var testLog = logger.NewLogger("test", logger.LogOutput{}, logger.LogLevelError)

var ctxDefault = tenant.WithID(context.Background(), tenant.Default)

// dataDir is a standalone data dir written through the OSS SQLite stores.
type dataDir struct {
	t   *testing.T
	dir string
}

func newDataDir(t *testing.T) *dataDir {
	return &dataDir{t: t, dir: t.TempDir()}
}

// with opens (creating and migrating, as the server does) the named SQLite file, runs fn and closes it again, so
// the import finds a stopped server's files.
func (d *dataDir) with(name string, migrations fs.FS, fn func(db *sqlx.DB)) {
	d.t.Helper()
	db, err := sqlite.New(filepath.Join(d.dir, name), migrations, sqlite.DataSourceOptions{WALEnabled: true})
	require.NoError(d.t, err)
	defer func() { require.NoError(d.t, db.Close()) }()
	fn(db)
}

// path of a file in the data dir.
func (d *dataDir) path(name string) string { return filepath.Join(d.dir, name) }

func (d *dataDir) write(name, content string) {
	d.t.Helper()
	require.NoError(d.t, os.WriteFile(d.path(name), []byte(content), 0o600))
}

// run imports the data dir into the default tenant of a fresh database.
func run(t *testing.T, pg *pgtest.DB, o Options) *Report {
	t.Helper()
	rep, err := Run(context.Background(), pg.App, o)
	require.NoError(t, err)
	return rep
}

func want(t *testing.T, rep *Report, name string, read, added, present, skipped int) {
	t.Helper()
	got := rep.Get(name)
	require.NotNil(t, got, "entity %q not in the report: %+v", name, rep.Entities)
	require.Equal(t, EntityCount{Name: name, Read: read, Added: added, Present: present, Skipped: skipped}, *got)
}

// sameJSON compares what want and got show in the API: times in different zones (or Location values) compare equal
// when they are the same instant written the same way.
func sameJSON(t *testing.T, want, got any) {
	t.Helper()
	w, err := json.Marshal(want)
	require.NoError(t, err)
	g, err := json.Marshal(got)
	require.NoError(t, err)
	require.JSONEq(t, string(w), string(g))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}
