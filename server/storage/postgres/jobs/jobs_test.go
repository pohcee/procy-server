package jobs_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ossjobs "github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/api/jobs/schedule"
	"github.com/openrport/openrport/server/repotest"
	"github.com/openrport/openrport/server/test/jb"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"

	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/jobs"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestJobsContract(t *testing.T) {
	repotest.Jobs(t, func(t *testing.T) ossjobs.Provider {
		d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
		return jobs.New(d.App)
	})
}

func TestSchedulesContract(t *testing.T) {
	repotest.Schedules(t, func(t *testing.T) repotest.SchedulesRepos {
		d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
		return repotest.SchedulesRepos{Jobs: jobs.New(d.App), Schedules: jobs.NewScheduleStore(d.App)}
	})
}

// fill stores a schedule with one run (a multi-job and its job) in each of the tenants a and b.
func fill(t *testing.T, d *pgtest.DB) {
	t.Helper()
	js, ss := jobs.New(d.App), jobs.NewScheduleStore(d.App)
	for _, tid := range []tenant.ID{repotest.TenantA, repotest.TenantB} {
		ctx := repotest.Ctx(tid)
		require.NoError(t, ss.Insert(ctx, &schedule.Schedule{Base: schedule.Base{ID: "s1", Name: string(tid), Type: schedule.TypeCommand}}))
		require.NoError(t, js.SaveMultiJob(ctx, jb.NewMulti(t).JID("mj1").ScheduleID("s1").Build()))
		require.NoError(t, js.SaveJob(ctx, jb.New(t).JID("j1").MultiJobID("mj1").Build()))
	}
}

var tables = []string{"jobs", "multi_jobs", "schedules"}

func TestRLSHidesOtherTenants(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	fill(t, d)
	for _, table := range tables {
		var tenants []string
		require.NoError(t, postgres.InTenant(repotest.Ctx(repotest.TenantA), d.App, func(tx *sqlx.Tx) error {
			return tx.Select(&tenants, "SELECT tenant_id FROM "+table)
		}))
		assert.Equal(t, []string{"a"}, tenants, table)

		var n int
		require.NoError(t, d.App.Get(&n, "SELECT count(*) FROM "+table))
		assert.Zero(t, n, "%s without a tenant", table)
		require.NoError(t, d.Owner.Get(&n, "SELECT count(*) FROM "+table))
		assert.Equal(t, 2, n, table)
	}
}

func TestTenantDeleteCascades(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	fill(t, d)
	_, err := d.Owner.Exec("DELETE FROM tenants WHERE id = 'a'")
	require.NoError(t, err)
	for _, table := range tables {
		var tenants []string
		require.NoError(t, d.Owner.Select(&tenants, "SELECT tenant_id FROM "+table))
		assert.Equal(t, []string{"b"}, tenants, table)
	}
}

// The details columns hold exactly what the OSS Go types marshal, so rows copied from SQLite read back the same.
func TestDetailsAreGoJSON(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	ctx := repotest.Ctx(repotest.TenantA)
	js := jobs.New(d.App)
	mj := jb.NewMulti(t).JID("mj1").Build()
	mj.LibraryEntryID = "e1"
	require.NoError(t, js.SaveMultiJob(ctx, mj))
	job := jb.New(t).JID("j1").MultiJobID("mj1").Build()
	require.NoError(t, js.CreateJob(ctx, job))
	lib := *job
	lib.LibraryEntryID, lib.LibraryVersion, lib.OS = "e1", 2, "linux"
	require.NoError(t, js.CreateJob(ctx, &lib))

	var got string
	require.NoError(t, d.Owner.Get(&got, "SELECT details FROM jobs WHERE jid = 'j1'"))
	want, err := (&ossjobs.JobDetails{
		Command: job.Command, PID: job.PID, TimeoutSec: job.TimeoutSec, Result: job.Result, ClientName: job.ClientName,
		LibraryEntryID: "e1", LibraryVersion: 2, OS: "linux",
	}).Value()
	require.NoError(t, err)
	assert.Equal(t, want, got)

	require.NoError(t, d.Owner.Get(&got, "SELECT details FROM multi_jobs WHERE jid = 'mj1'"))
	assert.Equal(t, `{"client_ids":`+mustJSON(t, mj.ClientIDs)+`,"group_ids":null,"tags":null,"command":"/bin/date;foo;whoami","interpreter":"","cwd":"","is_sudo":false,"timeout_sec":60,"concurrent":false,"abort_on_err":false,"library_entry_id":"e1"}`, got, "key order and spacing as Go marshals")

	// rows as SQLite stores them (other key order, extra spaces) read back
	_, err = d.Owner.Exec(`INSERT INTO jobs (tenant_id, jid, status, started_at, created_by, client_id, details)
		VALUES ('a', 'imported', 'successful', '2020-10-10 10:10:10+00', 'admin', 'c1', '{"pid": null, "command": "ls", "result": {"summary": "ok"}}')`)
	require.NoError(t, err)
	imported, err := js.GetByJID(ctx, "c1", "imported")
	require.NoError(t, err)
	require.NotNil(t, imported)
	assert.Equal(t, "ls", imported.Command)
	assert.Equal(t, "ok", imported.Result.Summary)
	assert.Equal(t, time.Date(2020, 10, 10, 10, 10, 10, 0, time.UTC), imported.StartedAt)
}

func mustJSON(t *testing.T, v interface{}) string {
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// Postgres checks a job's multi-job reference; the server always saves the multi-job first.
func TestJobOfUnknownMultiJobRefused(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	js := jobs.New(d.App)
	require.NoError(t, js.SaveMultiJob(repotest.Ctx(repotest.TenantB), jb.NewMulti(t).JID("mj1").Build()))
	err := js.SaveJob(repotest.Ctx(repotest.TenantA), jb.New(t).MultiJobID("mj1").Build())
	assert.Error(t, err, "the multi-job of another tenant is not a reference")
	n, err := js.Count(repotest.Ctx(repotest.TenantA), &query.ListOptions{})
	require.NoError(t, err)
	assert.Zero(t, n)
}

// Times come back in UTC whatever the session time zone.
func TestTimesInUTC(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	ctx := repotest.Ctx(repotest.TenantA)
	js := jobs.New(d.App)
	berlin := time.FixedZone("CET", 3600)
	started := time.Date(2022, 1, 1, 10, 0, 0, 0, berlin)
	require.NoError(t, js.SaveJob(ctx, jb.New(t).JID("j1").ClientID("c1").StartedAt(started).FinishedAt(started.Add(time.Minute)).Build()))
	got, err := js.GetByJID(ctx, "c1", "j1")
	require.NoError(t, err)
	assert.Equal(t, started.UTC(), got.StartedAt)
	assert.Equal(t, started.Add(time.Minute).UTC(), *got.FinishedAt)
}

func TestStoresWithoutTenantTouchNothing(t *testing.T) {
	// a nil database: any access would panic
	js, ss := jobs.New(nil), jobs.NewScheduleStore(nil)
	_, err := js.List(context.Background(), nil)
	assert.Error(t, err)
	_, err = ss.List(context.Background(), nil)
	assert.Error(t, err)
	assert.NoError(t, js.Close())
}
