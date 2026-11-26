package repotest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/api/jobs"
	"github.com/pohcee/procy-server/server/test/jb"
	"github.com/pohcee/procy-server/share/models"
	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/tenant"
)

// Jobs checks a jobs.Provider: jobs and multi-jobs, their lists, counts and cleanup.
//
// A job's multi_job_id always names a multi-job saved before it, as the server does; an implementation may
// enforce that reference.
func Jobs(t *testing.T, newRepo Factory[jobs.Provider]) {
	t.Run("GetByJIDOnlyFindsTheClientsOwnJob", func(t *testing.T) {
		p := newRepo(t)
		ctx := tenant.WithID(context.Background(), tenant.Default)
		job := jb.New(t).JID("j-of-c2").ClientID("c2").Build()
		require.NoError(t, p.CreateJob(ctx, job))

		got, err := p.GetByJID(ctx, "c1", "j-of-c2")
		require.NoError(t, err)
		assert.Nil(t, got, "a job of another client must not be found through this client")

		got, err = p.GetByJID(ctx, "c2", "j-of-c2")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "c2", got.ClientID)
	})

	t.Run("MissingTenant", func(t *testing.T) {
		p := newRepo(t)
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
	})

	t.Run("JobRoundTrip", func(t *testing.T) {
		p := newRepo(t)
		ctx := Ctx(TenantA)

		job1 := jb.New(t).Status(models.JobStatusRunning).Result(nil).IsSudo().Build()
		job2 := jb.New(t).ClientID(job1.ClientID).Cwd("/root").Build()
		job2.Interpreter, job2.IsScript, job2.Error = "powershell", true, "some error"
		job2.LibraryEntryID, job2.LibraryVersion, job2.OS = "e1", 4, "windows"
		job2.Result = &models.JobResult{StdOut: "out", StdErr: "err", Summary: "sum"}
		job3 := jb.New(t).Build()
		job3.PID = nil
		for _, j := range []*models.Job{job1, job2, job3} {
			require.NoError(t, p.SaveJob(ctx, j))
		}
		for _, j := range []*models.Job{job1, job2, job3} {
			got, err := p.GetByJID(ctx, j.ClientID, j.JID)
			require.NoError(t, err)
			assert.Equal(t, j, got)
		}

		got, err := p.GetByJID(ctx, job3.ClientID, "unknown-jid")
		require.NoError(t, err)
		assert.Nil(t, got)

		// SaveJob replaces the whole job
		job1.Status = models.JobStatusSuccessful
		job1.Result = &models.JobResult{StdOut: "some std out", StdErr: "some std err"}
		ft := time.Date(2020, 11, 5, 12, 11, 20, 0, time.UTC)
		job1.FinishedAt = &ft
		job1.IsSudo = false
		require.NoError(t, p.SaveJob(ctx, job1))
		got, err = p.GetByJID(ctx, job1.ClientID, job1.JID)
		require.NoError(t, err)
		assert.Equal(t, job1, got)

		listed, err := p.List(ctx, &query.ListOptions{Filters: []query.FilterOption{{Column: []string{"client_id"}, Values: []string{job1.ClientID}}}})
		require.NoError(t, err)
		assert.ElementsMatch(t, []*models.Job{job1, job2}, listed)
		listed, err = p.List(ctx, &query.ListOptions{Filters: []query.FilterOption{{Column: []string{"client_id"}, Values: []string{"unknown-cid"}}}})
		require.NoError(t, err)
		assert.Empty(t, listed)
	})

	t.Run("JobOfMultiJobHasSchedule", func(t *testing.T) {
		p := newRepo(t)
		ctx := Ctx(TenantA)
		mj := jb.NewMulti(t).JID("mj1").ScheduleID("s1").Build()
		require.NoError(t, p.SaveMultiJob(ctx, mj))
		job := jb.New(t).JID("j1").MultiJobID("mj1").Build()
		require.NoError(t, p.SaveJob(ctx, job))

		got, err := p.GetByJID(ctx, job.ClientID, "j1")
		require.NoError(t, err)
		require.NotNil(t, got)
		require.NotNil(t, got.ScheduleID)
		assert.Equal(t, "s1", *got.ScheduleID)
		require.NotNil(t, got.MultiJobID)
		assert.Equal(t, "mj1", *got.MultiJobID)

		listed, err := p.List(ctx, &query.ListOptions{Filters: []query.FilterOption{{Column: []string{"schedule_id"}, Values: []string{"s1"}}}})
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, "j1", listed[0].JID)
	})

	t.Run("ListFiltersSortsAndCount", func(t *testing.T) {
		p := newRepo(t)
		ctx := Ctx(TenantA)
		base := time.Date(2021, 3, 1, 10, 0, 0, 0, time.UTC)
		require.NoError(t, p.SaveMultiJob(ctx, jb.NewMulti(t).JID("mj1").ScheduleID("s1").Build()))
		fin := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
		js := []*models.Job{
			jb.New(t).JID("j1").ClientID("c1").StartedAt(base).FinishedAt(fin(5)).Build(),
			jb.New(t).JID("j2").ClientID("c1").StartedAt(base.Add(time.Hour)).Status(models.JobStatusRunning).Build(),
			jb.New(t).JID("j3").ClientID("c2").StartedAt(base.Add(2 * time.Hour)).FinishedAt(fin(150)).MultiJobID("mj1").Status(models.JobStatusFailed).Build(),
			jb.New(t).JID("j4").ClientID("c2").StartedAt(base.Add(3 * time.Hour)).FinishedAt(fin(5)).Build(),
		}
		js[3].CreatedBy = "other-user"
		for _, j := range js {
			require.NoError(t, p.SaveJob(ctx, j))
		}
		jids := func(list []*models.Job) []string {
			res := make([]string, 0, len(list))
			for _, j := range list {
				res = append(res, j.JID)
			}
			return res
		}
		list := func(o *query.ListOptions) []string {
			t.Helper()
			res, err := p.List(ctx, o)
			require.NoError(t, err)
			return jids(res)
		}
		count := func(o *query.ListOptions) int {
			t.Helper()
			n, err := p.Count(ctx, o)
			require.NoError(t, err)
			return n
		}
		filter := func(col string, op query.FilterOperatorType, values ...string) query.FilterOption {
			return query.FilterOption{Column: []string{col}, Operator: op, Values: values}
		}

		// default order: finished_at descending (unfinished last), then jid
		assert.Equal(t, []string{"j3", "j1", "j4", "j2"}, list(&query.ListOptions{}))
		assert.Equal(t, 4, count(&query.ListOptions{}))

		for name, c := range map[string]struct {
			filter query.FilterOption
			want   []string
		}{
			"client_id":          {filter("client_id", "", "c2"), []string{"j3", "j4"}},
			"client_id values":   {filter("client_id", "", "c1", "c2"), []string{"j1", "j2", "j3", "j4"}},
			"status":             {filter("status", "", models.JobStatusRunning), []string{"j2"}},
			"jid":                {filter("jid", "", "j4"), []string{"j4"}},
			"jid wildcard":       {filter("jid", "", "J*"), []string{"j1", "j2", "j3", "j4"}},
			"created_by":         {filter("created_by", "", "other-user"), []string{"j4"}},
			"multi_job_id":       {filter("multi_job_id", "", "mj1"), []string{"j3"}},
			"schedule_id":        {filter("schedule_id", "", "s1"), []string{"j3"}},
			"started_at gt":      {filter("started_at", query.FilterOperatorTypeGT, "2021-03-01 11:00:00+00:00"), []string{"j3", "j4"}},
			"started_at since":   {filter("started_at", query.FilterOperatorTypeSince, "2021-03-01 11:00:00+00:00"), []string{"j2", "j3", "j4"}},
			"started_at lt":      {filter("started_at", query.FilterOperatorTypeLT, "2021-03-01 11:00:00+00:00"), []string{"j1"}},
			"started_at until":   {filter("started_at", query.FilterOperatorTypeUntil, "2021-03-01 11:00:00+00:00"), []string{"j1", "j2"}},
			"finished_at gt":     {filter("finished_at", query.FilterOperatorTypeGT, "2021-03-01 10:05:00+00:00"), []string{"j3"}},
			"finished_at until":  {filter("finished_at", query.FilterOperatorTypeUntil, "2021-03-01 10:05:00+00:00"), []string{"j1", "j4"}},
			"no match":           {filter("client_id", "", "c3"), []string{}},
			"multi_job_id empty": {filter("multi_job_id", "", ""), []string{"j1", "j2", "j4"}},
		} {
			o := &query.ListOptions{Filters: []query.FilterOption{c.filter}}
			assert.ElementsMatch(t, c.want, list(o), name)
			assert.Equal(t, len(c.want), count(o), name)
		}

		two := []query.FilterOption{filter("client_id", "", "c1"), filter("status", "", models.JobStatusSuccessful)}
		assert.Equal(t, []string{"j1"}, list(&query.ListOptions{Filters: two}))
		assert.Equal(t, 1, count(&query.ListOptions{Filters: two}))

		sorted := func(col string, asc bool) []string {
			return list(&query.ListOptions{Sorts: []query.SortOption{{Column: col, IsASC: asc}, {Column: "jid", IsASC: true}}})
		}
		assert.Equal(t, []string{"j1", "j2", "j3", "j4"}, sorted("started_at", true))
		assert.Equal(t, []string{"j4", "j3", "j2", "j1"}, sorted("started_at", false))
		assert.Equal(t, []string{"j4", "j3", "j2", "j1"}, sorted("jid", false))
		assert.Equal(t, []string{"j3", "j2", "j1", "j4"}, sorted("status", true))
		assert.Equal(t, []string{"j1", "j2", "j3", "j4"}, sorted("created_by", false))
		// a missing value sorts lowest, as in SQL engines that order NULL first
		assert.Equal(t, []string{"j2", "j1", "j4", "j3"}, sorted("finished_at", true))
		assert.Equal(t, []string{"j3", "j1", "j4", "j2"}, sorted("finished_at", false))
		assert.Equal(t, []string{"j1", "j2", "j4", "j3"}, sorted("multi_job_id", true))
		assert.Equal(t, []string{"j3", "j1", "j2", "j4"}, sorted("schedule_id", false))

		// pagination limits the list, not the count
		paged := &query.ListOptions{
			Sorts:      []query.SortOption{{Column: "jid", IsASC: true}},
			Pagination: query.NewPagination(2, 1),
		}
		assert.Equal(t, []string{"j2", "j3"}, list(paged))
		assert.Equal(t, 4, count(paged))
	})

	t.Run("CreateJob", func(t *testing.T) {
		p := newRepo(t)
		ctx := Ctx(TenantA)

		job := jb.New(t).Status(models.JobStatusSuccessful).Result(nil).Build()
		require.NoError(t, p.CreateJob(ctx, job))
		got, err := p.GetByJID(ctx, job.ClientID, job.JID)
		require.NoError(t, err)
		assert.Equal(t, job, got)

		// creating it again keeps the stored job
		again := *job
		again.Status = models.JobStatusRunning
		require.NoError(t, p.CreateJob(ctx, &again))
		got, err = p.GetByJID(ctx, job.ClientID, job.JID)
		require.NoError(t, err)
		assert.Equal(t, job, got)
	})

	t.Run("CreateJobPatchesLibraryFieldsOfExistingJob", func(t *testing.T) {
		p := newRepo(t)
		ctx := Ctx(TenantA)

		// a fast result saved the job first, without library fields
		job := jb.New(t).Status(models.JobStatusSuccessful).Result(&models.JobResult{Summary: "done"}).Build()
		require.NoError(t, p.CreateJob(ctx, job))

		late := *job
		late.Status = models.JobStatusRunning
		late.Result = nil
		late.Command = "changed"
		late.LibraryEntryID, late.LibraryVersion, late.OS = "e1", 3, "linux"
		require.NoError(t, p.CreateJob(ctx, &late))

		got, err := p.GetByJID(ctx, job.ClientID, job.JID)
		require.NoError(t, err)
		want := *job
		want.LibraryEntryID, want.LibraryVersion, want.OS = "e1", 3, "linux"
		assert.Equal(t, &want, got, "only the library fields change")
	})

	t.Run("MultiJobs", func(t *testing.T) {
		p := newRepo(t)
		ctx := Ctx(TenantA)

		sums, err := p.GetMultiJobSummaries(ctx, &query.ListOptions{})
		require.NoError(t, err)
		assert.Empty(t, sums)

		t1 := time.Date(2022, 5, 5, 12, 0, 0, 0, time.UTC)
		job1 := jb.NewMulti(t).JID("1111").StartedAt(t1.Add(-time.Hour)).WithJobs().WithSudo().AbortOnErr(true).Build()
		job1.GroupIDs = []string{"g1"}
		job1.ClientTags = &models.JobClientTags{Tags: []string{"linux"}, Operator: "OR"}
		job1.LibraryEntryID, job1.LibraryVersion, job1.Error = "e1", 2, "some error"
		job2 := jb.NewMulti(t).JID("2222").StartedAt(t1).WithCwd("/root").ScheduleID("s1").Build() // jid used to check the order
		job3 := jb.NewMulti(t).JID("3333").StartedAt(t1).Concurrent(true).Build()
		job3.CreatedBy = "other-user"
		require.NoError(t, p.SaveMultiJob(ctx, job1))
		for _, j := range job1.Jobs {
			require.NoError(t, p.SaveJob(ctx, j))
		}
		require.NoError(t, p.SaveMultiJob(ctx, job2))
		require.NoError(t, p.SaveMultiJob(ctx, job3))

		for _, mj := range []*models.MultiJob{job1, job2, job3} {
			got, err := p.GetMultiJob(ctx, mj.JID)
			require.NoError(t, err)
			assert.Equal(t, mj, got)
		}
		got, err := p.GetMultiJob(ctx, "unknown-jid")
		require.NoError(t, err)
		assert.Nil(t, got)

		children, err := p.List(ctx, &query.ListOptions{Filters: []query.FilterOption{{Column: []string{"multi_job_id"}, Values: []string{job1.JID}}}})
		require.NoError(t, err)
		assert.ElementsMatch(t, job1.Jobs, children)

		// summaries: started_at descending, then jid
		sums, err = p.GetMultiJobSummaries(ctx, &query.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, []*models.MultiJobSummary{&job2.MultiJobSummary, &job3.MultiJobSummary, &job1.MultiJobSummary}, sums)
		n, err := p.CountMultiJobs(ctx, &query.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, 3, n)

		summaryJIDs := func(o *query.ListOptions) []string {
			t.Helper()
			res, err := p.GetMultiJobSummaries(ctx, o)
			require.NoError(t, err)
			ids := []string{}
			for _, s := range res {
				ids = append(ids, s.JID)
			}
			count, err := p.CountMultiJobs(ctx, o)
			require.NoError(t, err)
			if o.Pagination == nil {
				assert.Equal(t, len(ids), count)
			}
			return ids
		}
		filter := func(col string, op query.FilterOperatorType, values ...string) *query.ListOptions {
			return &query.ListOptions{Filters: []query.FilterOption{{Column: []string{col}, Operator: op, Values: values}}}
		}
		assert.ElementsMatch(t, []string{"3333"}, summaryJIDs(filter("created_by", "", "other-user")))
		assert.ElementsMatch(t, []string{"2222"}, summaryJIDs(filter("schedule_id", "", "s1")))
		assert.ElementsMatch(t, []string{"1111"}, summaryJIDs(filter("jid", "", "1111")))
		assert.ElementsMatch(t, []string{"2222", "3333"}, summaryJIDs(filter("started_at", query.FilterOperatorTypeGT, "2022-05-05 11:30:00+00:00")))
		assert.ElementsMatch(t, []string{"1111"}, summaryJIDs(filter("started_at", query.FilterOperatorTypeUntil, "2022-05-05 11:00:00+00:00")))
		assert.Equal(t, []string{"1111", "2222", "3333"}, summaryJIDs(&query.ListOptions{Sorts: []query.SortOption{{Column: "jid", IsASC: true}}}))
		assert.Equal(t, []string{"1111", "2222", "3333"}, summaryJIDs(&query.ListOptions{Sorts: []query.SortOption{{Column: "started_at", IsASC: true}, {Column: "jid", IsASC: true}}}))
		assert.Equal(t, []string{"2222"}, summaryJIDs(&query.ListOptions{
			Sorts:      []query.SortOption{{Column: "jid", IsASC: true}},
			Pagination: query.NewPagination(1, 1),
		}))
		n, err = p.CountMultiJobs(ctx, &query.ListOptions{Pagination: query.NewPagination(1, 1)})
		require.NoError(t, err)
		assert.Equal(t, 3, n, "the count ignores pagination")

		// SaveMultiJob updates an existing one
		job1.Interpreter = "cmd"
		job1.Concurrent = true
		job1.StartedAt = t1.Add(time.Second)
		require.NoError(t, p.SaveMultiJob(ctx, job1))
		got, err = p.GetMultiJob(ctx, job1.JID)
		require.NoError(t, err)
		assert.Equal(t, job1, got)
		sums, err = p.GetMultiJobSummaries(ctx, &query.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, []*models.MultiJobSummary{&job1.MultiJobSummary, &job2.MultiJobSummary, &job3.MultiJobSummary}, sums)
	})

	t.Run("Cleanup", func(t *testing.T) {
		p := newRepo(t)
		ctx := Ctx(TenantA)

		mj1 := jb.NewMulti(t).Build()
		mj2 := jb.NewMulti(t).Build()
		mj3 := jb.NewMulti(t).Build()
		for _, mj := range []*models.MultiJob{mj1, mj2, mj3} {
			require.NoError(t, p.SaveMultiJob(ctx, mj))
		}
		start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
		at := func(i int) time.Time { return start.Add(time.Duration(i) * time.Minute) }
		js := []*models.Job{
			jb.New(t).MultiJobID(mj1.JID).StartedAt(at(1)).Build(),
			jb.New(t).MultiJobID(mj1.JID).StartedAt(at(2)).Build(),
			jb.New(t).StartedAt(at(3)).Build(),
			jb.New(t).MultiJobID(mj2.JID).StartedAt(at(4)).Build(),
			jb.New(t).StartedAt(at(5)).Build(),
			jb.New(t).MultiJobID(mj2.JID).StartedAt(at(6)).Build(),
			jb.New(t).MultiJobID(mj3.JID).StartedAt(at(7)).Build(),
			jb.New(t).MultiJobID(mj3.JID).StartedAt(at(8)).Build(),
		}
		for _, j := range js {
			require.NoError(t, p.SaveJob(ctx, j))
		}

		require.NoError(t, p.CleanupJobsMultiJobs(ctx, 3))

		// multi-jobs with an older job go with all their jobs; then only the newest of the rest stay
		for mj, kept := range map[*models.MultiJob]bool{mj1: false, mj2: false, mj3: true} {
			got, err := p.GetMultiJob(ctx, mj.JID)
			require.NoError(t, err)
			assert.Equal(t, kept, got != nil, mj.JID)
		}
		for i, kept := range []bool{false, false, false, false, true, false, true, true} {
			got, err := p.GetByJID(ctx, js[i].ClientID, js[i].JID)
			require.NoError(t, err)
			assert.Equal(t, kept, got != nil, "job %d", i+1)
		}
	})

	t.Run("TenantIsolation", func(t *testing.T) {
		p := newRepo(t)
		a, b := Ctx(TenantA), Ctx(TenantB)

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
		n, err = p.CountMultiJobs(b, &query.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, 1, n)

		// SaveJob of the same JID replaces only the tenant's own job
		jobA.Status = models.JobStatusFailed
		require.NoError(t, p.SaveJob(a, jobA))
		got, err = p.GetByJID(b, "c1", "j1")
		require.NoError(t, err)
		assert.Equal(t, models.JobStatusSuccessful, got.Status)

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

		// a job created only in tenant b is invisible to a
		require.NoError(t, p.CreateJob(b, jb.New(t).JID("only-b").ClientID("c1").Build()))
		got, err = p.GetByJID(a, "c1", "only-b")
		require.NoError(t, err)
		assert.Nil(t, got)

		// cleanup counts and deletes per tenant
		require.NoError(t, p.SaveJob(a, jb.New(t).JID("j2").ClientID("c1").StartedAt(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)).Build()))
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
		n, err = p.Count(b, &query.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, 2, n)
	})
}
