package jobs

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/db/migration/jobs"
	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/server/test/jb"
	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/tenant"
)

// versionBeforeTenant is the last jobs.db migration without tenant_id.
const versionBeforeTenant = 3

func ctxFor(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

func newTenantTestProvider(t *testing.T) *SqliteProvider {
	t.Helper()
	db, err := sqlite.New(":memory:", jobs.FS, DataSourceOptions)
	require.NoError(t, err)
	p := NewSqliteProvider(db, testLog)
	t.Cleanup(func() { p.Close() })
	return p
}

// migrateTo opens a file DB and migrates it to the given schema version (up or down).
func migrateTo(t *testing.T, path string, version uint) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("sqlite3", path)
	require.NoError(t, err)
	require.NoError(t, sqlite.MigrateTo(db, jobs.FS, version))
	return db
}

func TestJobProviderRequiresTenant(t *testing.T) {
	p := newTenantTestProvider(t)
	ctx := context.Background()
	job := jb.New(t).Build()
	mj := jb.NewMulti(t).Build()

	_, err := p.GetByJID(ctx, job.ClientID, job.JID)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.List(ctx, &query.ListOptions{})
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.Count(ctx, &query.ListOptions{})
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.ErrorIs(t, p.SaveJob(ctx, job), tenant.ErrMissing)
	assert.ErrorIs(t, p.CreateJob(ctx, job), tenant.ErrMissing)
	_, err = p.GetMultiJob(ctx, mj.JID)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.GetMultiJobSummaries(ctx, &query.ListOptions{})
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.CountMultiJobs(ctx, &query.ListOptions{})
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.ErrorIs(t, p.SaveMultiJob(ctx, mj), tenant.ErrMissing)
	assert.ErrorIs(t, p.CleanupJobsMultiJobs(ctx, 1), tenant.ErrMissing)
}

func TestJobProviderIsolatesTenants(t *testing.T) {
	p := newTenantTestProvider(t)
	a, b := ctxFor("a"), ctxFor("b")

	mjA := jb.NewMulti(t).JID("mj1").Build()
	mjB := jb.NewMulti(t).JID("mj1").Build()
	mjB.Command = "from b"
	require.NoError(t, p.SaveMultiJob(a, mjA))
	require.NoError(t, p.SaveMultiJob(b, mjB))

	jobA := jb.New(t).JID("j1").ClientID("c1").MultiJobID("mj1").ClientName("from a").Build()
	jobB := jb.New(t).JID("j1").ClientID("c1").MultiJobID("mj1").ClientName("from b").Build()
	require.NoError(t, p.SaveJob(a, jobA))
	require.NoError(t, p.SaveJob(b, jobB))

	got, err := p.GetByJID(a, "c1", "j1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "from a", got.ClientName)
	got, err = p.GetByJID(b, "c1", "j1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "from b", got.ClientName)
	got, err = p.GetByJID(ctxFor("c"), "c1", "j1")
	require.NoError(t, err)
	assert.Nil(t, got)

	listed, err := p.List(a, &query.ListOptions{Filters: []query.FilterOption{{Column: []string{"client_id"}, Values: []string{"c1"}}}})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, "from a", listed[0].ClientName)
	n, err := p.Count(b, &query.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	gotMJ, err := p.GetMultiJob(b, "mj1")
	require.NoError(t, err)
	require.NotNil(t, gotMJ)
	assert.Equal(t, "from b", gotMJ.Command)
	require.Len(t, gotMJ.Jobs, 1)
	assert.Equal(t, "from b", gotMJ.Jobs[0].ClientName)

	sums, err := p.GetMultiJobSummaries(a, &query.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, sums, 1)
	n, err = p.CountMultiJobs(ctxFor("c"), &query.ListOptions{})
	require.NoError(t, err)
	assert.Zero(t, n)

	// CreateJob of an existing job patches the library fields of its own tenant only
	lib := jb.New(t).JID("j1").ClientID("c1").Build()
	lib.LibraryEntryID, lib.LibraryVersion, lib.OS = "e1", 2, "linux"
	require.NoError(t, p.CreateJob(a, lib))
	got, err = p.GetByJID(a, "c1", "j1")
	require.NoError(t, err)
	assert.Equal(t, "e1", got.LibraryEntryID)
	got, err = p.GetByJID(b, "c1", "j1")
	require.NoError(t, err)
	assert.Empty(t, got.LibraryEntryID)

	// a job created only in tenant c does not collide with the same JID elsewhere
	require.NoError(t, p.CreateJob(ctxFor("c"), jb.New(t).JID("j1").ClientID("c1").Build()))

	// cleanup counts and deletes per tenant
	require.NoError(t, p.SaveJob(a, jb.New(t).JID("j2").ClientID("c1").StartedAt(time.Now()).Build()))
	require.NoError(t, p.CleanupJobsMultiJobs(a, 1))
	got, err = p.GetByJID(a, "c1", "j1")
	require.NoError(t, err)
	assert.Nil(t, got, "tenant a keeps only its newest job")
	gotMJ, err = p.GetMultiJob(a, "mj1")
	require.NoError(t, err)
	assert.Nil(t, gotMJ)
	got, err = p.GetByJID(b, "c1", "j1")
	require.NoError(t, err)
	assert.NotNil(t, got, "tenant b is untouched by a's cleanup")
	gotMJ, err = p.GetMultiJob(b, "mj1")
	require.NoError(t, err)
	assert.NotNil(t, gotMJ)
}

func TestUpgradeKeepsJobsInDefaultTenant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	old := migrateTo(t, path, versionBeforeTenant)
	_, err := old.Exec(`INSERT INTO multi_jobs (jid, started_at, created_by, details, schedule_id) VALUES ('mj1', '2020-10-10 10:10:10', 'admin', '{"command":"ls"}', 's1')`)
	require.NoError(t, err)
	_, err = old.Exec(`INSERT INTO jobs (jid, status, started_at, created_by, client_id, multi_job_id, details) VALUES ('j1', 'successful', '2020-10-10 10:10:10', 'admin', 'c1', 'mj1', '{"command":"ls"}')`)
	require.NoError(t, err)
	_, err = old.Exec(`INSERT INTO schedules (id, created_at, created_by, name, schedule, type, details) VALUES ('s1', '2020-10-10 10:10:10', 'admin', 'n', '* * * * *', 'command', '{}')`)
	require.NoError(t, err)
	require.NoError(t, old.Close())

	db, err := sqlite.New(path, jobs.FS, DataSourceOptions)
	require.NoError(t, err)
	p := NewSqliteProvider(db, testLog)
	defer p.Close()

	def := ctxFor(tenant.Default)
	got, err := p.GetByJID(def, "c1", "j1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "ls", got.Command)
	require.NotNil(t, got.ScheduleID)
	assert.Equal(t, "s1", *got.ScheduleID)

	mj, err := p.GetMultiJob(def, "mj1")
	require.NoError(t, err)
	require.NotNil(t, mj)
	assert.Len(t, mj.Jobs, 1)

	var n int
	require.NoError(t, db.Get(&n, `SELECT COUNT(*) FROM schedules WHERE tenant_id = 'default' AND id = 's1'`))
	assert.Equal(t, 1, n)
}

func TestDowngradeKeepsOnlyDefaultTenantJobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	db, err := sqlite.New(path, jobs.FS, DataSourceOptions)
	require.NoError(t, err)
	p := NewSqliteProvider(db, testLog)
	for _, tid := range []tenant.ID{tenant.Default, "a"} {
		ctx := ctxFor(tid)
		require.NoError(t, p.SaveMultiJob(ctx, jb.NewMulti(t).JID("mj1").Build()))
		job := jb.New(t).JID("j1").MultiJobID("mj1").ClientName(string(tid)).Build()
		require.NoError(t, p.SaveJob(ctx, job))
		_, err = db.Exec(`INSERT INTO schedules (tenant_id, id, created_at, created_by, name, schedule, type, details) VALUES (?, 's1', '2020-10-10 10:10:10', 'admin', ?, '* * * * *', 'command', '{}')`, tid, string(tid))
		require.NoError(t, err)
	}
	require.NoError(t, db.Close())

	down := migrateTo(t, path, versionBeforeTenant)
	defer down.Close()

	var details []string
	require.NoError(t, down.Select(&details, `SELECT details FROM jobs`))
	require.Len(t, details, 1)
	assert.Contains(t, details[0], `"client_name":"default"`)
	var n int
	require.NoError(t, down.Get(&n, `SELECT COUNT(*) FROM multi_jobs`))
	assert.Equal(t, 1, n)
	var names []string
	require.NoError(t, down.Select(&names, `SELECT name FROM schedules`))
	assert.Equal(t, []string{"default"}, names)

	for _, table := range []string{"jobs", "multi_jobs", "schedules"} {
		require.NoError(t, down.Get(&n, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'tenant_id'`, table))
		assert.Zero(t, n, table)
	}
}

func TestJobJSONHasNoTenant(t *testing.T) {
	p := newTenantTestProvider(t)
	ctx := ctxFor("a")
	require.NoError(t, p.SaveJob(ctx, jb.New(t).JID("j1").ClientID("c1").Build()))
	got, err := p.GetByJID(ctx, "c1", "j1")
	require.NoError(t, err)
	assertNoTenantJSON(t, got)
	require.NoError(t, p.SaveMultiJob(ctx, jb.NewMulti(t).JID("mj1").Build()))
	mj, err := p.GetMultiJob(ctx, "mj1")
	require.NoError(t, err)
	assertNoTenantJSON(t, mj)
}

func assertNoTenantJSON(t *testing.T, v interface{}) {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "tenant")
}
