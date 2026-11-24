package repotest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/api/jobs/schedule"
	"github.com/openrport/openrport/server/test/jb"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
)

// SchedulesRepos is a schedule provider with the jobs provider on the same storage: a schedule's last run, its
// jobs in progress and the runs deleted with it come from the jobs and multi-jobs that provider stores.
type SchedulesRepos struct {
	Jobs      jobs.Provider
	Schedules schedule.Provider
}

func testSchedules() []*schedule.Schedule {
	return []*schedule.Schedule{
		{
			Base: schedule.Base{
				ID:        "1",
				Name:      "schedule 1",
				CreatedBy: "user1",
				CreatedAt: time.Date(2022, 1, 1, 1, 0, 0, 0, time.UTC),
				Schedule:  "* * * * *",
				Type:      schedule.TypeCommand,
			},
			Details: schedule.Details{
				ClientIDs: []string{"c1"},
				GroupIDs:  []string{"g1"},
				Command:   "/bin/true",
				Cwd:       "/home/rport",
				Overlaps:  true,
			},
		},
		{
			Base: schedule.Base{
				ID:        "2",
				Name:      "schedule 2",
				CreatedBy: "user2",
				CreatedAt: time.Date(2022, 1, 1, 2, 0, 0, 0, time.UTC),
				Schedule:  "*/5 * * * *",
				Type:      schedule.TypeScript,
			},
			Details: schedule.Details{
				ClientIDs:           []string{"c2"},
				GroupIDs:            []string{"g2"},
				ClientTags:          &models.JobClientTags{Tags: []string{"t1"}, Operator: "AND"},
				Script:              "echo 'test'",
				Interpreter:         "/bin/sh",
				Cwd:                 "/home/rport",
				IsSudo:              true,
				TimeoutSec:          3,
				ExecuteConcurrently: true,
				AbortOnError:        func() *bool { b := true; return &b }(),
				Overlaps:            true,
			},
		},
		{
			Base: schedule.Base{
				ID:        "3",
				Name:      "library schedule",
				CreatedBy: "user1",
				CreatedAt: time.Date(2022, 1, 1, 3, 0, 0, 0, time.UTC),
				Schedule:  "0 * * * *",
				Type:      schedule.TypeLibrary,
			},
			Details: schedule.Details{
				ClientIDs:      []string{"c1", "c2"},
				LibraryEntryID: "e1",
			},
		},
	}
}

// Schedules checks a schedule.Provider together with the jobs provider on the same storage.
func Schedules(t *testing.T, newRepos Factory[SchedulesRepos]) {
	withData := func(t *testing.T) (SchedulesRepos, []*schedule.Schedule) {
		t.Helper()
		r := newRepos(t)
		data := testSchedules()
		for _, s := range data {
			require.NoError(t, r.Schedules.Insert(Ctx(TenantA), s))
		}
		return r, data
	}

	t.Run("MissingTenant", func(t *testing.T) {
		p := newRepos(t).Schedules
		ctx := context.Background()
		s := testSchedules()[0]
		assert.ErrorIs(t, p.Insert(ctx, s), tenant.ErrMissing)
		assert.ErrorIs(t, p.Update(ctx, s), tenant.ErrMissing)
		_, err := p.List(ctx, nil)
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = p.Get(ctx, "1")
		assert.ErrorIs(t, err, tenant.ErrMissing)
		assert.ErrorIs(t, p.Delete(ctx, "1"), tenant.ErrMissing)
		_, err = p.CountJobsInProgress(ctx, "1", 60)
		assert.ErrorIs(t, err, tenant.ErrMissing)
	})

	t.Run("InsertGet", func(t *testing.T) {
		r, data := withData(t)
		ctx := Ctx(TenantA)
		for _, s := range data {
			got, err := r.Schedules.Get(ctx, s.ID)
			require.NoError(t, err)
			assert.Equal(t, s, got)
		}
		got, err := r.Schedules.Get(ctx, "-2")
		require.NoError(t, err)
		assert.Nil(t, got)

		assert.Error(t, r.Schedules.Insert(ctx, data[0]), "the ID is taken")
	})

	t.Run("List", func(t *testing.T) {
		r, data := withData(t)
		ctx := Ctx(TenantA)

		all, err := r.Schedules.List(ctx, nil)
		require.NoError(t, err)
		assert.ElementsMatch(t, data, all)
		all, err = r.Schedules.List(ctx, &query.ListOptions{})
		require.NoError(t, err)
		assert.ElementsMatch(t, data, all)

		ids := func(o *query.ListOptions) []string {
			t.Helper()
			res, err := r.Schedules.List(ctx, o)
			require.NoError(t, err)
			out := []string{}
			for _, s := range res {
				out = append(out, s.ID)
			}
			return out
		}
		filter := func(col string, op query.FilterOperatorType, values ...string) *query.ListOptions {
			return &query.ListOptions{Filters: []query.FilterOption{{Column: []string{col}, Operator: op, Values: values}}}
		}
		assert.ElementsMatch(t, []string{"2"}, ids(filter("id", "", "2")))
		assert.ElementsMatch(t, []string{"1", "3"}, ids(filter("created_by", "", "user1")))
		assert.ElementsMatch(t, []string{"3"}, ids(filter("type", "", schedule.TypeLibrary)))
		assert.ElementsMatch(t, []string{"1", "2"}, ids(filter("name", "", "SCHEDULE*")))
		assert.ElementsMatch(t, []string{"1", "3"}, ids(filter("name", "", "schedule 1", "library schedule")))
		assert.ElementsMatch(t, []string{"2", "3"}, ids(filter("created_at", query.FilterOperatorTypeGT, "2022-01-01 01:30:00+00:00")))
		assert.Empty(t, ids(filter("name", "", "nope")))

		sorted := func(col string, asc bool) []string {
			return ids(&query.ListOptions{Sorts: []query.SortOption{{Column: col, IsASC: asc}, {Column: "id", IsASC: true}}})
		}
		assert.Equal(t, []string{"3", "2", "1"}, sorted("id", false))
		assert.Equal(t, []string{"1", "2", "3"}, sorted("created_at", true))
		assert.Equal(t, []string{"1", "3", "2"}, sorted("created_by", true))
		assert.Equal(t, []string{"3", "1", "2"}, sorted("name", true))
		assert.Equal(t, []string{"2", "3", "1"}, sorted("type", false))
		assert.Equal(t, []string{"2"}, ids(&query.ListOptions{
			Sorts:      []query.SortOption{{Column: "id", IsASC: true}},
			Pagination: query.NewPagination(1, 1),
		}))
	})

	t.Run("Update", func(t *testing.T) {
		r, data := withData(t)
		ctx := Ctx(TenantA)

		updated := *data[0]
		updated.Schedule = "1 5 * * *"
		updated.Name = "renamed"
		updated.Type = schedule.TypeScript
		updated.Details.Command = ""
		updated.Details.Script = "ls"
		// created_at and created_by are kept
		updated.CreatedBy = "someone else"
		updated.CreatedAt = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
		require.NoError(t, r.Schedules.Update(ctx, &updated))

		got, err := r.Schedules.Get(ctx, "1")
		require.NoError(t, err)
		want := updated
		want.CreatedBy, want.CreatedAt = data[0].CreatedBy, data[0].CreatedAt
		assert.Equal(t, &want, got)
		got, err = r.Schedules.Get(ctx, "2")
		require.NoError(t, err)
		assert.Equal(t, data[1], got)
	})

	t.Run("LastExecution", func(t *testing.T) {
		r, _ := withData(t)
		ctx := Ctx(TenantA)
		last := func(id string) *schedule.Execution {
			t.Helper()
			s, err := r.Schedules.Get(ctx, id)
			require.NoError(t, err)
			require.NotNil(t, s)
			listed, err := r.Schedules.List(ctx, &query.ListOptions{Filters: []query.FilterOption{{Column: []string{"id"}, Values: []string{id}}}})
			require.NoError(t, err)
			require.Len(t, listed, 1)
			assert.Equal(t, s, listed[0], "List and Get agree")
			return s.LastExecution
		}
		base := time.Date(2022, 2, 1, 0, 0, 0, 0, time.UTC)

		assert.Nil(t, last("1"), "no runs yet")

		// schedule 1: an older multi-client run and a newer single-client run
		old := jb.NewMulti(t).JID("old").ScheduleID("1").StartedAt(base).Build()
		require.NoError(t, r.Jobs.SaveMultiJob(ctx, old))
		require.NoError(t, r.Jobs.SaveJob(ctx, jb.New(t).MultiJobID("old").Status(models.JobStatusFailed).Build()))
		require.NoError(t, r.Jobs.SaveJob(ctx, jb.New(t).MultiJobID("old").Status(models.JobStatusSuccessful).Build()))
		ex := last("1")
		require.NotNil(t, ex)
		assert.Equal(t, base, *ex.StartedAt)
		assert.Equal(t, 2, *ex.ClientCount)
		assert.Equal(t, 1, *ex.SuccessCount)
		assert.Nil(t, ex.Status, "status only for single-client runs")
		assert.Nil(t, ex.Summary)
		assert.Nil(t, ex.Error)

		newer := jb.NewMulti(t).JID("newer").ScheduleID("1").StartedAt(base.Add(time.Hour)).Build()
		require.NoError(t, r.Jobs.SaveMultiJob(ctx, newer))
		require.NoError(t, r.Jobs.SaveJob(ctx, jb.New(t).MultiJobID("newer").Status(models.JobStatusSuccessful).Result(&models.JobResult{Summary: "all good"}).Build()))
		require.NoError(t, r.Jobs.SaveJob(ctx, jb.New(t).MultiJobID("newer").Status(models.JobStatusSkipped).Result(&models.JobResult{Summary: "no macOS implementation"}).Build()))
		// a newer multi-job without jobs and without error is not a run
		require.NoError(t, r.Jobs.SaveMultiJob(ctx, jb.NewMulti(t).JID("empty").ScheduleID("1").StartedAt(base.Add(2*time.Hour)).Build()))
		// runs of other schedules don't count
		require.NoError(t, r.Jobs.SaveMultiJob(ctx, jb.NewMulti(t).JID("other").ScheduleID("2").StartedAt(base.Add(3*time.Hour)).Build()))
		require.NoError(t, r.Jobs.SaveJob(ctx, jb.New(t).MultiJobID("other").Status(models.JobStatusRunning).Build()))

		ex = last("1")
		require.NotNil(t, ex)
		assert.Equal(t, base.Add(time.Hour), *ex.StartedAt)
		assert.Equal(t, 1, *ex.ClientCount, "skipped jobs are not counted")
		assert.Equal(t, 1, *ex.SuccessCount)
		require.NotNil(t, ex.Status)
		assert.Equal(t, models.JobStatusSuccessful, *ex.Status, "a skipped job doesn't mask the success")
		require.NotNil(t, ex.Summary)
		assert.Equal(t, "all good", *ex.Summary)
		assert.Nil(t, ex.Error)

		ex = last("2")
		require.NotNil(t, ex)
		assert.Equal(t, models.JobStatusRunning, *ex.Status)
		assert.Equal(t, 0, *ex.SuccessCount)

		// schedule 3: a run that could not start records its error without jobs
		failed := jb.NewMulti(t).JID("failed").ScheduleID("3").StartedAt(base).Build()
		failed.Error = "entry deleted"
		require.NoError(t, r.Jobs.SaveMultiJob(ctx, failed))
		ex = last("3")
		require.NotNil(t, ex)
		require.NotNil(t, ex.Status)
		assert.Equal(t, models.JobStatusFailed, *ex.Status)
		require.NotNil(t, ex.Error)
		assert.Equal(t, "entry deleted", *ex.Error)
		assert.Equal(t, 0, *ex.ClientCount)

		// a newer run with only skipped jobs: no clients ran it
		require.NoError(t, r.Jobs.SaveMultiJob(ctx, jb.NewMulti(t).JID("skipped").ScheduleID("3").StartedAt(base.Add(time.Hour)).Build()))
		require.NoError(t, r.Jobs.SaveJob(ctx, jb.New(t).MultiJobID("skipped").Status(models.JobStatusSkipped).Result(&models.JobResult{Summary: "skip"}).Build()))
		ex = last("3")
		require.NotNil(t, ex)
		assert.Equal(t, base.Add(time.Hour), *ex.StartedAt)
		assert.Equal(t, 0, *ex.ClientCount)
		assert.Equal(t, 0, *ex.SuccessCount)
		assert.Nil(t, ex.Status, "status only for single-client runs")
		assert.Nil(t, ex.Error)
	})

	t.Run("CountJobsInProgress", func(t *testing.T) {
		r, data := withData(t)
		ctx := Ctx(TenantA)

		mj := jb.NewMulti(t).ScheduleID(data[0].ID).Build()
		other := jb.NewMulti(t).ScheduleID(data[1].ID).Build()
		require.NoError(t, r.Jobs.SaveMultiJob(ctx, mj))
		require.NoError(t, r.Jobs.SaveMultiJob(ctx, other))
		now := time.Now().UTC().Truncate(time.Second)
		for _, j := range []*models.Job{
			jb.New(t).MultiJobID(mj.JID).FinishedAt(now).Build(),                       // finished
			jb.New(t).MultiJobID(mj.JID).StartedAt(now).Build(),                        // in progress
			jb.New(t).MultiJobID(mj.JID).StartedAt(now.Add(-10 * time.Second)).Build(), // in progress
			jb.New(t).MultiJobID(mj.JID).StartedAt(now.Add(-2 * time.Minute)).Build(),  // timed out
			jb.New(t).MultiJobID(other.JID).StartedAt(now).Build(),                     // other schedule
			jb.New(t).StartedAt(now).Build(),                                           // no schedule
		} {
			require.NoError(t, r.Jobs.SaveJob(ctx, j))
		}

		n, err := r.Schedules.CountJobsInProgress(ctx, data[0].ID, 60)
		require.NoError(t, err)
		assert.Equal(t, 2, n)
		n, err = r.Schedules.CountJobsInProgress(ctx, data[0].ID, 600)
		require.NoError(t, err)
		assert.Equal(t, 3, n)
		n, err = r.Schedules.CountJobsInProgress(ctx, data[1].ID, 60)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		n, err = r.Schedules.CountJobsInProgress(ctx, "unknown", 60)
		require.NoError(t, err)
		assert.Zero(t, n)
	})

	t.Run("Delete", func(t *testing.T) {
		r, data := withData(t)
		ctx := Ctx(TenantA)

		mj1 := jb.NewMulti(t).ScheduleID(data[0].ID).Build()
		mj2 := jb.NewMulti(t).ScheduleID(data[1].ID).Build()
		mj3 := jb.NewMulti(t).ScheduleID(data[1].ID).Build()
		single := jb.NewMulti(t).Build()
		for _, mj := range []*models.MultiJob{mj1, mj2, mj3, single} {
			require.NoError(t, r.Jobs.SaveMultiJob(ctx, mj))
		}
		for _, id := range []string{mj1.JID, mj1.JID, mj2.JID, mj3.JID, single.JID} {
			require.NoError(t, r.Jobs.SaveJob(ctx, jb.New(t).MultiJobID(id).Build()))
		}
		require.NoError(t, r.Jobs.SaveJob(ctx, jb.New(t).Build()))

		assert.EqualError(t, r.Schedules.Delete(ctx, "-2"), "cannot find entry by id -2")

		require.NoError(t, r.Schedules.Delete(ctx, data[1].ID))
		got, err := r.Schedules.Get(ctx, data[1].ID)
		require.NoError(t, err)
		assert.Nil(t, got)

		// the schedule's runs go with it, the others stay
		n, err := r.Jobs.Count(ctx, &query.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, 4, n)
		for mj, kept := range map[*models.MultiJob]bool{mj1: true, mj2: false, mj3: false, single: true} {
			got, err := r.Jobs.GetMultiJob(ctx, mj.JID)
			require.NoError(t, err)
			assert.Equal(t, kept, got != nil, mj.JID)
		}
		n, err = r.Jobs.CountMultiJobs(ctx, &query.ListOptions{Filters: []query.FilterOption{{Column: []string{"schedule_id"}, Values: []string{data[1].ID}}}})
		require.NoError(t, err)
		assert.Zero(t, n)

		got, err = r.Schedules.Get(ctx, data[0].ID)
		require.NoError(t, err)
		assert.NotNil(t, got)
		assert.Error(t, r.Schedules.Delete(ctx, data[1].ID), "already deleted")
	})

	t.Run("TenantIsolation", func(t *testing.T) {
		r := newRepos(t)
		p, jp := r.Schedules, r.Jobs
		a, b := Ctx(TenantA), Ctx(TenantB)
		named := func(id, name string) *schedule.Schedule {
			s := testSchedules()[0]
			s.ID, s.Name = id, name
			return s
		}
		require.NoError(t, p.Insert(a, named("s1", "from a")))
		require.NoError(t, p.Insert(b, named("s1", "from b")))
		require.NoError(t, p.Insert(b, named("s2", "only b")))

		// the same multi-job and job IDs in both tenants; only b's job is still running
		now := time.Now().UTC().Truncate(time.Second)
		for _, c := range []struct {
			ctx    context.Context
			status string
		}{{a, models.JobStatusSuccessful}, {b, models.JobStatusRunning}} {
			require.NoError(t, jp.SaveMultiJob(c.ctx, jb.NewMulti(t).JID("mj1").ScheduleID("s1").Build()))
			job := jb.New(t).JID("j1").MultiJobID("mj1").Status(c.status).StartedAt(now).Build()
			if c.status == models.JobStatusSuccessful {
				job = jb.New(t).JID("j1").MultiJobID("mj1").Status(c.status).StartedAt(now).FinishedAt(now).Build()
			}
			require.NoError(t, jp.SaveJob(c.ctx, job))
		}

		got, err := p.Get(a, "s1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "from a", got.Name)
		require.NotNil(t, got.LastExecution)
		require.NotNil(t, got.LastExecution.ClientCount)
		assert.Equal(t, 1, *got.LastExecution.ClientCount, "last execution counts only the tenant's own jobs")
		assert.Equal(t, models.JobStatusSuccessful, *got.LastExecution.Status)
		got, err = p.Get(b, "s1")
		require.NoError(t, err)
		assert.Equal(t, models.JobStatusRunning, *got.LastExecution.Status)
		got, err = p.Get(a, "s2")
		require.NoError(t, err)
		assert.Nil(t, got)

		listed, err := p.List(a, &query.ListOptions{})
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, "from a", listed[0].Name)
		listed, err = p.List(b, nil)
		require.NoError(t, err)
		assert.Len(t, listed, 2)

		n, err := p.CountJobsInProgress(a, "s1", 60)
		require.NoError(t, err)
		assert.Zero(t, n)
		n, err = p.CountJobsInProgress(b, "s1", 60)
		require.NoError(t, err)
		assert.Equal(t, 1, n)

		require.NoError(t, p.Update(a, named("s1", "a updated")))
		require.NoError(t, p.Update(a, named("s2", "a can't reach it")))
		got, err = p.Get(b, "s1")
		require.NoError(t, err)
		assert.Equal(t, "from b", got.Name)
		got, err = p.Get(b, "s2")
		require.NoError(t, err)
		assert.Equal(t, "only b", got.Name)

		assert.Error(t, p.Delete(a, "s2"), "another tenant's schedule is not found")
		require.NoError(t, p.Delete(a, "s1"))
		got, err = p.Get(a, "s1")
		require.NoError(t, err)
		assert.Nil(t, got)
		mj, err := jp.GetMultiJob(a, "mj1")
		require.NoError(t, err)
		assert.Nil(t, mj, "deleting a schedule removes its tenant's runs")
		mj, err = jp.GetMultiJob(b, "mj1")
		require.NoError(t, err)
		require.NotNil(t, mj, "other tenant's runs stay")
		assert.Len(t, mj.Jobs, 1)
		got, err = p.Get(b, "s1")
		require.NoError(t, err)
		assert.NotNil(t, got)
	})
}
