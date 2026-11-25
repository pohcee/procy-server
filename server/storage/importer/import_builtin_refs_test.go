package importer

import (
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	jobsmigration "github.com/openrport/openrport/db/migration/jobs"
	librarymigration "github.com/openrport/openrport/db/migration/library"
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/api/jobs/schedule"
	"github.com/openrport/openrport/server/library"
	pgjobs "github.com/openrport/openrport/server/storage/postgres/jobs"
	pglibrary "github.com/openrport/openrport/server/storage/postgres/library"
	"github.com/openrport/openrport/server/storage/postgres/pgserver"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/tenant"
)

// builtinRefsSource is a data dir whose schedules and jobs run built-in library entries (and one customer entry),
// as a standalone server makes them: the built-ins got their ids when the source synced them.
type builtinRefsSource struct {
	d                    *dataDir
	clientUpdate, osUpds string // source ids of the built-ins
}

func writeBuiltinRefsSource(t *testing.T) *builtinRefsSource {
	t.Helper()
	src := &builtinRefsSource{d: newDataDir(t)}
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	src.d.with("library.db", librarymigration.FS, func(db *sqlx.DB) {
		s := library.NewSQLiteStore(db)
		builtins, err := library.Builtins()
		require.NoError(t, err)
		_, _, _, err = library.SyncBuiltins(ctxDefault, s, builtins, "1.0.0", now)
		require.NoError(t, err)
		cu, err := s.ByBuiltinKey(ctxDefault, "client-update")
		require.NoError(t, err)
		ou, err := s.ByBuiltinKey(ctxDefault, "os-updates")
		require.NoError(t, err)
		src.clientUpdate, src.osUpds = cu.ID, ou.ID
		c1 := library.Content{Name: "disk usage", Implementations: map[string]library.Implementation{"linux": {Kind: library.KindCommand, Code: "df -h", TimeoutSec: 60}}}
		require.NoError(t, s.Insert(ctxDefault, library.Entry{ID: "custom1", Name: c1.Name, ActiveVersion: 1, OS: []string{"linux"}, CreatedAt: now, CreatedBy: "admin", UpdatedAt: now, UpdatedBy: "admin"},
			library.Version{EntryID: "custom1", Version: 1, CreatedAt: now, CreatedBy: "admin", Content: &c1}))
	})
	src.d.with("jobs.db", jobsmigration.FS, func(db *sqlx.DB) {
		s := schedule.NewSQLiteProvider(db)
		for id, entry := range map[string]string{"s-cu": src.clientUpdate, "s-ou": src.osUpds, "s-custom": "custom1"} {
			require.NoError(t, s.Insert(ctxDefault, &schedule.Schedule{
				Base:    schedule.Base{ID: id, CreatedAt: now, CreatedBy: "admin", Name: id, Schedule: "0 3 * * *", Type: "command"},
				Details: schedule.Details{ClientIDs: []string{"c1"}, Command: "x <&> y", TimeoutSec: 60, LibraryEntryID: entry},
			}))
		}
		p := jobs.NewSqliteProvider(db, testLog)
		sched := "s-cu"
		require.NoError(t, p.SaveMultiJob(ctxDefault, &models.MultiJob{
			MultiJobSummary: models.MultiJobSummary{JID: "mj1", StartedAt: now, CreatedBy: "admin", ScheduleID: &sched},
			ClientIDs:       []string{"c1"}, Command: "update", TimeoutSec: 60, LibraryEntryID: src.clientUpdate, LibraryVersion: 1,
		}))
		mid := "mj1"
		require.NoError(t, p.CreateJob(ctxDefault, &models.Job{JID: "j1", Status: models.JobStatusSuccessful, ClientID: "c1", Command: "update",
			StartedAt: now, CreatedBy: "admin", MultiJobID: &mid, LibraryEntryID: src.clientUpdate, LibraryVersion: 1}))
		require.NoError(t, p.CreateJob(ctxDefault, &models.Job{JID: "j2", Status: models.JobStatusSuccessful, ClientID: "c1", Command: "df -h",
			StartedAt: now, CreatedBy: "admin", LibraryEntryID: "custom1", LibraryVersion: 1}))
	})
	return src
}

// A new tenant has its own built-ins under new ids: references of the source's schedules and jobs to the source's
// built-ins are rewritten to the tenant's entries; a customer entry keeps its id.
func TestImportRewritesBuiltinLibraryRefs(t *testing.T) {
	pg := pgtest.New(t)
	src := writeBuiltinRefsSource(t)
	require.NoError(t, pgserver.AddTenant(ctxDefault, pg.Owner, "acme", "Acme"))
	ctx := tenant.WithID(ctxDefault, "acme")
	lib := pglibrary.New(pg.App)
	cu, err := lib.ByBuiltinKey(ctx, "client-update")
	require.NoError(t, err)
	ou, err := lib.ByBuiltinKey(ctx, "os-updates")
	require.NoError(t, err)
	require.NotEqual(t, src.clientUpdate, cu.ID, "the tenant's built-ins should have their own ids")

	check := func() {
		t.Helper()
		ss := pgjobs.NewScheduleStore(pg.App)
		for id, entry := range map[string]string{"s-cu": cu.ID, "s-ou": ou.ID, "s-custom": "custom1"} {
			got, err := ss.Get(ctx, id)
			require.NoError(t, err)
			require.NotNil(t, got, id)
			assert.Equal(t, entry, got.Details.LibraryEntryID, id)
			assert.Equal(t, "x <&> y", got.Details.Command)
			_, err = lib.Get(ctx, got.Details.LibraryEntryID)
			require.NoError(t, err, "schedule %s references an entry the tenant doesn't have", id)
		}
		js := pgjobs.New(pg.App)
		mj, err := js.GetMultiJob(ctx, "mj1")
		require.NoError(t, err)
		assert.Equal(t, cu.ID, mj.LibraryEntryID)
		assert.Equal(t, 1, mj.LibraryVersion)
		j1, err := js.GetByJID(ctx, "c1", "j1")
		require.NoError(t, err)
		assert.Equal(t, cu.ID, j1.LibraryEntryID)
		j2, err := js.GetByJID(ctx, "c1", "j2")
		require.NoError(t, err)
		assert.Equal(t, "custom1", j2.LibraryEntryID)
	}

	// a dry run reports what it would rewrite and writes nothing
	rep := run(t, pg, Options{From: src.d.dir, Tenant: "acme", WithHistory: true, DryRun: true})
	want(t, rep, entSchedules, 3, 3, 0, 0)
	assert.True(t, hasWarning(rep, "client-update "+src.clientUpdate+" → "+cu.ID), rep.Warnings)
	assert.Contains(t, rep.Warnings, "schedules: 2 run built-in library entries the target has under other ids, their references point at the target's entries")
	none, err := pgjobs.NewScheduleStore(pg.App).Get(ctx, "s-cu")
	require.NoError(t, err)
	assert.Nil(t, none)

	rep = run(t, pg, Options{From: src.d.dir, Tenant: "acme", WithHistory: true})
	want(t, rep, entSchedules, 3, 3, 0, 0)
	want(t, rep, entMultiJobs, 1, 1, 0, 0)
	want(t, rep, entJobs, 2, 2, 0, 0)
	assert.Contains(t, rep.Warnings, "schedules: 2 run built-in library entries the target has under other ids, their references point at the target's entries")
	assert.Contains(t, rep.Warnings, "multi jobs: 1 run built-in library entries the target has under other ids, their references point at the target's entries")
	assert.Contains(t, rep.Warnings, "jobs: 1 run built-in library entries the target has under other ids, their references point at the target's entries")
	check()

	// a second run changes nothing
	rep = run(t, pg, Options{From: src.d.dir, Tenant: "acme", WithHistory: true})
	check()
	want(t, rep, entSchedules, 3, 0, 3, 0)
	want(t, rep, entMultiJobs, 1, 0, 1, 0)
	want(t, rep, entJobs, 2, 0, 2, 0)
}

// An import made before the references were rewritten left schedules that reference the source's built-in ids: a
// new run repairs them.
func TestImportRepairsBuiltinRefsOfEarlierImport(t *testing.T) {
	pg := pgtest.New(t)
	src := writeBuiltinRefsSource(t)
	require.NoError(t, pgserver.AddTenant(ctxDefault, pg.Owner, "acme", "Acme"))
	ctx := tenant.WithID(ctxDefault, "acme")
	cu, err := pglibrary.New(pg.App).ByBuiltinKey(ctx, "client-update")
	require.NoError(t, err)
	// the schedule as the earlier import wrote it
	ss := pgjobs.NewScheduleStore(pg.App)
	require.NoError(t, ss.Insert(ctx, &schedule.Schedule{
		Base:    schedule.Base{ID: "s-cu", CreatedAt: time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC), CreatedBy: "admin", Name: "s-cu", Schedule: "0 3 * * *", Type: "command"},
		Details: schedule.Details{ClientIDs: []string{"c1"}, Command: "x <&> y", TimeoutSec: 60, LibraryEntryID: src.clientUpdate},
	}))

	rep := run(t, pg, Options{From: src.d.dir, Tenant: "acme"})
	want(t, rep, entSchedules, 3, 2, 1, 0)
	assert.True(t, hasWarning(rep, "1 already imported"), rep.Warnings)
	got, err := ss.Get(ctx, "s-cu")
	require.NoError(t, err)
	assert.Equal(t, cu.ID, got.Details.LibraryEntryID)
}

func TestLibraryRemapRewrite(t *testing.T) {
	var m libraryRemap
	m.add("src", "dst", "client-update")
	for _, tc := range []struct{ in, want string }{
		{`{"library_entry_id":"src","command":"a <b> & c","n":1.50}`, `{"command":"a <b> & c","library_entry_id":"dst","n":1.50}`},
		{`{"library_entry_id":"custom","command":"x"}`, ""},
		{`{"command":"library_entry_id"}`, ""},
		{`{"library_entry_id":5}`, ""},
		{`not json library_entry_id`, ""},
	} {
		got, ok := m.rewrite(tc.in)
		if tc.want == "" {
			assert.False(t, ok, tc.in)
			assert.Equal(t, tc.in, got)
			continue
		}
		assert.True(t, ok, tc.in)
		assert.Equal(t, tc.want, got)
	}
	got, ok := (&libraryRemap{}).rewrite(`{"library_entry_id":"src"}`)
	assert.False(t, ok)
	assert.Equal(t, `{"library_entry_id":"src"}`, got)
}
