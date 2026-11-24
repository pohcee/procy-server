package importer

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apisessionsmigration "github.com/openrport/openrport/db/migration/api_sessions"
	apitokenmigration "github.com/openrport/openrport/db/migration/api_token"
	clientgroupsmigration "github.com/openrport/openrport/db/migration/client_groups"
	clientsmigration "github.com/openrport/openrport/db/migration/clients"
	jobsmigration "github.com/openrport/openrport/db/migration/jobs"
	librarymigration "github.com/openrport/openrport/db/migration/library"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/api/authorization"
	"github.com/openrport/openrport/server/api/jobs/schedule"
	"github.com/openrport/openrport/server/api/session"
	"github.com/openrport/openrport/server/cgroups"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clients/storedtunnels"
	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/server/repotest"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	"github.com/openrport/openrport/share/tenant"
)

// fullDataDir writes every entity a standalone server keeps, and a config naming a users file and a client auth
// file. It returns the data dir and the config path.
func fullDataDir(t *testing.T) (*dataDir, string) {
	t.Helper()
	d := newDataDir(t)
	now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	d.with("clients.db", clientsmigration.FS, func(db *sqlx.DB) {
		require.NoError(t, clients.NewSQLiteStore(db, nil).Save(ctxDefault, repotest.StoreClient("c1", &now)))
		require.NoError(t, storedtunnels.NewSQLiteProvider(db).Insert(ctxDefault, &storedtunnels.StoredTunnel{ID: "t1", ClientID: "c1", CreatedAt: now, Name: "web"}))
	})
	d.with("client_groups.db", clientgroupsmigration.FS, func(db *sqlx.DB) {
		s, err := cgroups.NewSqliteProvider(db)
		require.NoError(t, err)
		require.NoError(t, s.Create(ctxDefault, &cgroups.ClientGroup{ID: "g1", Description: "one", Params: &cgroups.ClientParams{}}))
	})
	d.with("api_token.db", apitokenmigration.FS, func(db *sqlx.DB) {
		require.NoError(t, authorization.NewSqliteProvider(db).Save(ctxDefault, &authorization.APIToken{Username: "admin", Prefix: "pref1234", Name: "ci", Scope: authorization.APITokenRead, Token: "$2y$x", CreatedAt: &now}))
	})
	d.with("api_sessions.db", apisessionsmigration.FS, func(db *sqlx.DB) {
		_, err := session.NewSqliteProvider(db).Save(ctxDefault, session.APISession{Username: "admin", ExpiresAt: now.Add(time.Hour), LastAccessAt: now})
		require.NoError(t, err)
	})
	d.with("library.db", librarymigration.FS, func(db *sqlx.DB) {
		builtins, err := library.Builtins()
		require.NoError(t, err)
		_, _, _, err = library.SyncBuiltins(ctxDefault, library.NewSQLiteStore(db), builtins, "1.0.0", now)
		require.NoError(t, err)
	})
	d.with("jobs.db", jobsmigration.FS, func(db *sqlx.DB) {
		require.NoError(t, schedule.NewSQLiteProvider(db).Insert(ctxDefault, &schedule.Schedule{
			Base: schedule.Base{ID: "s1", CreatedAt: now, CreatedBy: "admin", Name: "n", Schedule: "* * * * *", Type: "command"},
		}))
	})
	writeHistory(t, d)
	writeVault(t, d)
	d.write("api-auth.json", fmt.Sprintf(`[{"username": "admin", "password": %q, "groups": ["Administrators"]}]`, hash(t, "pass")))
	d.write("client-auth.json", `{"dev-1": "plain-1"}`)
	conf := filepath.Join(t.TempDir(), "rportd.conf")
	writeFile(t, conf, fmt.Sprintf("[server]\ndata_dir = %q\nauth_file = \"client-auth.json\"\n[api]\nauth_file = \"api-auth.json\"\n", d.dir))
	// relative paths are relative to the config file
	require.NoError(t, os.Rename(d.path("api-auth.json"), filepath.Join(filepath.Dir(conf), "api-auth.json")))
	require.NoError(t, os.Rename(d.path("client-auth.json"), filepath.Join(filepath.Dir(conf), "client-auth.json")))
	return d, conf
}

func TestImportIdempotent(t *testing.T) {
	pg := pgtest.New(t)
	d, conf := fullDataDir(t)
	// From is optional with a config: its data_dir is used
	first := run(t, pg, Options{FromConfig: conf, WithHistory: true})
	for _, e := range first.Entities {
		assert.Equal(t, e.Read, e.Added+e.Skipped, "%+v", e)
		if e.Name != entGroupPermissions && e.Name != entLibraryMeta { // a users file has no permissions; no legacy import
			assert.Positive(t, e.Read, "%+v: the fixture should hold some", e)
		}
	}
	second := run(t, pg, Options{From: d.dir, FromConfig: conf, WithHistory: true})
	require.Equal(t, len(first.Entities), len(second.Entities))
	for i, e := range second.Entities {
		assert.Zero(t, e.Added, "%+v", e)
		switch e.Name {
		case entAuditLog, entNotifications:
			// no natural key: copied only into an empty table
			assert.Equal(t, EntityCount{Name: e.Name, Read: e.Read, Skipped: e.Read}, e)
		case entJobs:
			assert.Equal(t, first.Entities[i].Skipped, e.Skipped, "the orphan job")
			assert.Equal(t, e.Read-e.Skipped, e.Present)
		default:
			assert.Equal(t, EntityCount{Name: e.Name, Read: e.Read, Present: e.Read}, e)
		}
	}
	assert.True(t, hasWarning(second, "audit log: the target tenant already has auditlog rows"), second.Warnings)
}

func TestImportDryRunWritesNothing(t *testing.T) {
	pg := pgtest.New(t)
	d, conf := fullDataDir(t)
	dry := run(t, pg, Options{From: d.dir, FromConfig: conf, WithHistory: true, DryRun: true})
	assert.True(t, dry.DryRun)
	var rows int
	require.NoError(t, pg.Owner.Get(&rows, `SELECT (SELECT count(*) FROM clients) + (SELECT count(*) FROM users) + (SELECT count(*) FROM clients_auth)
		+ (SELECT count(*) FROM library_entries) + (SELECT count(*) FROM vault_values) + (SELECT count(*) FROM vault_status)
		+ (SELECT count(*) FROM jobs) + (SELECT count(*) FROM auditlog) + (SELECT count(*) FROM api_sessions)`))
	assert.Zero(t, rows)
	var last int64
	require.NoError(t, pg.Owner.Get(&last, "SELECT last_value FROM api_sessions_session_id_seq"))
	assert.Equal(t, int64(1), last, "a dry run doesn't move sequences")

	// the dry run reports what the real run then does
	real := run(t, pg, Options{From: d.dir, FromConfig: conf, WithHistory: true})
	assert.Equal(t, dry.Entities, real.Entities)
}

func TestImportUnknownTenant(t *testing.T) {
	pg := pgtest.New(t)
	_, err := Run(context.Background(), pg.App, Options{From: newDataDir(t).dir, Tenant: "nope"})
	require.EqualError(t, err, "tenant nope does not exist; create it with rportd tenant add")
}

func TestImportOtherTenantInSource(t *testing.T) {
	pg := pgtest.New(t)
	d := newDataDir(t)
	d.with("clients.db", clientsmigration.FS, func(db *sqlx.DB) {
		s := clients.NewSQLiteStore(db, nil)
		require.NoError(t, s.Save(ctxDefault, repotest.StoreClient("c1", nil)))
		require.NoError(t, s.Save(tenant.WithID(ctxDefault, "acme"), repotest.StoreClient("c2", nil)))
	})
	_, err := Run(context.Background(), pg.App, Options{From: d.dir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `clients.db clients has rows of tenant(s) ["acme"]`)
	var n int
	require.NoError(t, pg.Owner.Get(&n, "SELECT count(*) FROM clients"))
	assert.Zero(t, n)
}

// TestHelperHoldDatabase is not a test: TestImportRefusesHeldDataDir runs it in a child process that keeps a
// SQLite file open, as a running server does.
func TestHelperHoldDatabase(t *testing.T) {
	path := os.Getenv("IMPORT_TEST_HOLD_DB")
	if path == "" {
		t.Skip("helper process only")
	}
	db, err := sqlite.Open(path, sqlite.DataSourceOptions{WALEnabled: true})
	require.NoError(t, err)
	_, err = db.Exec("CREATE TABLE IF NOT EXISTS held (a)")
	require.NoError(t, err)
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n') // until the parent closes stdin
	_ = db.Close()
}

func TestImportRefusesHeldDataDir(t *testing.T) {
	pg := pgtest.New(t)
	d := newDataDir(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldDatabase$")
	cmd.Env = append(os.Environ(), "IMPORT_TEST_HOLD_DB="+d.path("clients.db"), pgtest.EnvDatabaseURL+"=")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", line)

	_, err = Run(context.Background(), pg.App, Options{From: d.dir})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stop rportd first")

	// the lock alone is found too (when /proc doesn't show the process, e.g. another user's)
	if !lockCheckSupported {
		return
	}
	err = checkLocks(d.dir, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is locked by pid")
}
