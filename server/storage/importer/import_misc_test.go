package importer

import (
	"crypto/sha256"
	"os"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apisessionsmigration "github.com/openrport/openrport/db/migration/api_sessions"
	apitokenmigration "github.com/openrport/openrport/db/migration/api_token"
	jobsmigration "github.com/openrport/openrport/db/migration/jobs"
	librarymigration "github.com/openrport/openrport/db/migration/library"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/api/authorization"
	"github.com/openrport/openrport/server/api/jobs/schedule"
	"github.com/openrport/openrport/server/api/session"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/library"
	pgapitokens "github.com/openrport/openrport/server/storage/postgres/apitokens"
	pgjobs "github.com/openrport/openrport/server/storage/postgres/jobs"
	pglibrary "github.com/openrport/openrport/server/storage/postgres/library"
	"github.com/openrport/openrport/server/storage/postgres/pgserver"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	pgsessions "github.com/openrport/openrport/server/storage/postgres/sessions"
	pgusers "github.com/openrport/openrport/server/storage/postgres/users"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
)

func TestImportAPITokensAndSessions(t *testing.T) {
	pg := pgtest.New(t)
	d := newDataDir(t)
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	expires := created.Add(365 * 24 * time.Hour)
	var wantTokens []*authorization.APIToken
	d.with("api_token.db", apitokenmigration.FS, func(db *sqlx.DB) {
		s := authorization.NewSqliteProvider(db)
		require.NoError(t, s.Save(ctxDefault, &authorization.APIToken{Username: "admin", Prefix: "pref1234", Name: "ci", Scope: authorization.APITokenRead, Token: "$2y$hash1", CreatedAt: &created, ExpiresAt: &expires}))
		require.NoError(t, s.Save(ctxDefault, &authorization.APIToken{Username: "admin", Prefix: "pref5678", Name: "other", Scope: authorization.APITokenReadWrite, Token: "$2y$hash2", CreatedAt: &created}))
		var err error
		wantTokens, err = s.GetAll(ctxDefault, "admin")
		require.NoError(t, err)
		// a token of a user who is neither imported nor in the target is not copied
		require.NoError(t, s.Save(ctxDefault, &authorization.APIToken{Username: "ghost", Prefix: "ghost123", Name: "ci", Scope: authorization.APITokenRead, Token: "$2y$hash3", CreatedAt: &created}))
	})
	var wantSessions []session.APISession
	d.with("api_sessions.db", apisessionsmigration.FS, func(db *sqlx.DB) {
		s := session.NewSqliteProvider(db)
		for i := 0; i < 3; i++ {
			_, err := s.Save(ctxDefault, session.APISession{Username: "admin", ExpiresAt: expires, LastAccessAt: created, UserAgent: "ua", IPAddress: "192.0.2.1"})
			require.NoError(t, err)
		}
		// a higher id, as left after sessions were deleted
		_, err := db.Exec("UPDATE api_sessions SET session_id = 40 WHERE session_id = 3")
		require.NoError(t, err)
		wantSessions, err = s.GetAll(ctxDefault)
		require.NoError(t, err)
		_, err = s.Save(ctxDefault, session.APISession{Username: "ghost", ExpiresAt: expires, LastAccessAt: created})
		require.NoError(t, err)
	})
	// without --from-config no users are imported: tokens and sessions are kept for the users the target has
	require.NoError(t, pgusers.New(pg.App).Add(ctxDefault, &users.User{Username: "admin", Password: "$2y$05$x"}))

	rep := run(t, pg, Options{From: d.dir})
	want(t, rep, entAPITokens, 3, 2, 0, 1)
	want(t, rep, entAPISessions, 4, 3, 0, 1)

	gotTokens, err := pgapitokens.New(pg.App).GetAll(ctxDefault, "admin")
	require.NoError(t, err)
	sameJSON(t, wantTokens, gotTokens)
	sessions := pgsessions.New(pg.App)
	gotSessions, err := sessions.GetAll(ctxDefault)
	require.NoError(t, err)
	sameJSON(t, wantSessions, gotSessions)
	assert.Equal(t, []int64{1, 2, 40}, []int64{gotSessions[0].SessionID, gotSessions[1].SessionID, gotSessions[2].SessionID})

	// a new session gets an id above the imported ones
	id, err := sessions.Save(ctxDefault, session.APISession{Username: "admin", ExpiresAt: expires, LastAccessAt: created})
	require.NoError(t, err)
	assert.Greater(t, id, int64(40))
}

func TestImportLibrary(t *testing.T) {
	pg := pgtest.New(t)
	d := newDataDir(t)
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	var wantEntries []library.Entry
	wantVersions := map[string][]library.Version{}
	d.with("library.db", librarymigration.FS, func(db *sqlx.DB) {
		s := library.NewSQLiteStore(db)
		builtins, err := library.Builtins()
		require.NoError(t, err)
		_, _, _, err = library.SyncBuiltins(ctxDefault, s, builtins, "1.0.0", now)
		require.NoError(t, err)
		c1 := library.Content{Name: "disk usage", Tags: []string{"ops"}, Implementations: map[string]library.Implementation{"linux": {Kind: library.KindCommand, Code: "df -h", TimeoutSec: 60}}}
		e := library.Entry{ID: "e1", Name: c1.Name, Tags: c1.Tags, ActiveVersion: 1, OS: []string{"linux"}, CreatedAt: now, CreatedBy: "admin", UpdatedAt: now, UpdatedBy: "admin"}
		require.NoError(t, s.Insert(ctxDefault, e, library.Version{EntryID: "e1", Version: 1, CreatedAt: now, CreatedBy: "admin", Changed: library.Changed(library.Content{}, c1), Content: &c1}))
		c2 := c1
		c2.Description = "with a description"
		e2 := e
		e2.Description, e2.ActiveVersion, e2.UpdatedAt = c2.Description, 2, now.Add(time.Hour)
		require.NoError(t, s.AddVersion(ctxDefault, e2, library.Version{EntryID: "e1", Version: 2, CreatedAt: now.Add(time.Hour), CreatedBy: "admin", Message: "describe", Changed: library.Changed(c1, c2), Content: &c2}, 1))
		gone := now.Add(2 * time.Hour)
		c3 := library.Content{Name: "old", Implementations: map[string]library.Implementation{"linux": {Kind: library.KindScript, Code: "echo", TimeoutSec: 60}}}
		require.NoError(t, s.Insert(ctxDefault, library.Entry{ID: "e2", Name: "old", ActiveVersion: 1, OS: []string{"linux"}, CreatedAt: now, CreatedBy: "admin", UpdatedAt: now, UpdatedBy: "admin"},
			library.Version{EntryID: "e2", Version: 1, CreatedAt: now, CreatedBy: "admin", Content: &c3}))
		require.NoError(t, s.SetDeleted(ctxDefault, "e2", &gone, "admin"))
		wantEntries, err = s.List(ctxDefault, true)
		require.NoError(t, err)
		for _, e := range wantEntries {
			vs, err := s.Versions(ctxDefault, e.ID)
			require.NoError(t, err)
			for _, v := range vs {
				full, err := s.Version(ctxDefault, e.ID, v.Version)
				require.NoError(t, err)
				wantVersions[e.ID] = append(wantVersions[e.ID], *full)
			}
		}
	})
	nBuiltins := len(wantEntries) - 2
	require.Positive(t, nBuiltins)

	rep := run(t, pg, Options{From: d.dir})
	want(t, rep, entLibraryEntries, nBuiltins+2, nBuiltins+2, 0, 0)
	want(t, rep, entLibraryVersions, nBuiltins+3, nBuiltins+3, 0, 0)

	store := pglibrary.New(pg.App)
	gotEntries, err := store.List(ctxDefault, true)
	require.NoError(t, err)
	sameJSON(t, wantEntries, gotEntries)
	for id, vs := range wantVersions {
		for _, v := range vs {
			got, err := store.Version(ctxDefault, id, v.Version)
			require.NoError(t, err)
			sameJSON(t, v, got)
		}
	}

	// a new tenant has the built-ins already (synced when it is created): they count as present
	require.NoError(t, pgserver.AddTenant(ctxDefault, pg.Owner, "acme", "Acme"))
	rep = run(t, pg, Options{From: d.dir, Tenant: "acme"})
	want(t, rep, entLibraryEntries, nBuiltins+2, 2, nBuiltins, 0)
	want(t, rep, entLibraryVersions, nBuiltins+3, 3, nBuiltins, 0)
	acme, err := store.List(tenant.WithID(ctxDefault, "acme"), true)
	require.NoError(t, err)
	assert.Len(t, acme, nBuiltins+2)
}

// An old library.db keeps scripts and commands in their own tables; the server converts them into entries on its
// first start after the upgrade. The import converts them in a copy and leaves the file as it was.
func TestImportLegacyLibrary(t *testing.T) {
	pg := pgtest.New(t)
	d := newDataDir(t)
	db, err := sqlite.Open(d.path("library.db"), sqlite.DataSourceOptions{})
	require.NoError(t, err)
	require.NoError(t, sqlite.MigrateTo(db, librarymigration.FS, 4))
	_, err = db.Exec(`INSERT INTO scripts (id, name, created_at, created_by, updated_at, updated_by, interpreter, is_sudo, cwd, script, tags, timeout_sec)
		VALUES ('s1', 'cleanup', '2023-05-06 07:08:09', 'admin', '2023-05-06 07:08:09', 'admin', '/bin/bash', 0, '/tmp', 'rm -rf /tmp/x', '["ops"]', 60)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO commands (id, name, created_at, created_by, updated_at, updated_by, cmd, tags, timeout_sec)
		VALUES ('k1', 'uptime', '2023-05-06 07:08:09', 'admin', '2023-05-06 07:08:09', 'admin', 'uptime', '[]', 30)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	before := fileSum(t, d.path("library.db"))

	rep := run(t, pg, Options{From: d.dir})
	want(t, rep, entLibraryEntries, 2, 2, 0, 0)
	assert.True(t, hasWarning(rep, "2 old scripts and commands were converted"), rep.Warnings)
	got, err := pglibrary.New(pg.App).List(ctxDefault, false)
	require.NoError(t, err)
	names := []string{got[0].Name, got[1].Name}
	assert.ElementsMatch(t, []string{"cleanup", "uptime"}, names)
	assert.Equal(t, before, fileSum(t, d.path("library.db")), "the source file was changed")
}

func fileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return sha256.Sum256(b)
}

func TestImportSchedules(t *testing.T) {
	pg := pgtest.New(t)
	d := newDataDir(t)
	var wantList []*schedule.Schedule
	d.with("jobs.db", jobsmigration.FS, func(db *sqlx.DB) {
		s := schedule.NewSQLiteProvider(db)
		abort := true
		require.NoError(t, s.Insert(ctxDefault, &schedule.Schedule{
			Base:    schedule.Base{ID: "s1", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), CreatedBy: "admin", Name: "nightly", Schedule: "0 3 * * *", Type: "command"},
			Details: schedule.Details{ClientIDs: []string{"c1"}, Command: "apt update", Interpreter: "/bin/sh", TimeoutSec: 60, AbortOnError: &abort},
		}))
		var err error
		wantList, err = s.List(ctxDefault, &query.ListOptions{})
		require.NoError(t, err)
	})
	rep := run(t, pg, Options{From: d.dir})
	want(t, rep, entSchedules, 1, 1, 0, 0)
	got, err := pgjobs.NewScheduleStore(pg.App).List(ctxDefault, &query.ListOptions{})
	require.NoError(t, err)
	sameJSON(t, wantList, got)
}
