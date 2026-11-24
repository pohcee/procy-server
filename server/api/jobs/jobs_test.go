package jobs

import (
	"os"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/migration/jobs"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/test/jb"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
)

var DataSourceOptions = sqlite.DataSourceOptions{WALEnabled: false}
var testLog = logger.NewLogger("jobs-test", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug)

func TestJobsSqliteProvider(t *testing.T) {
	ctx := ctxFor(tenant.Default)
	jobsDB, err := sqlite.New(":memory:", jobs.FS, DataSourceOptions)
	require.NoError(t, err)
	p := NewSqliteProvider(jobsDB, testLog)
	defer p.Close()

	// add jobs
	job1 := jb.New(t).Status(models.JobStatusRunning).Result(nil).IsSudo().Build()
	job2 := jb.New(t).ClientID(job1.ClientID).Cwd("/root").Build()
	job3 := jb.New(t).Build() // different client ID
	require.NoError(t, p.SaveJob(ctx, job1))
	require.NoError(t, p.SaveJob(ctx, job2))
	require.NoError(t, p.SaveJob(ctx, job3))

	// verify added jobs
	gotJob1, err := p.GetByJID(ctx, job1.ClientID, job1.JID)
	require.NoError(t, err)
	require.NotNil(t, gotJob1)
	assert.Equal(t, job1, gotJob1)

	gotJob2, err := p.GetByJID(ctx, job2.ClientID, job2.JID)
	require.NoError(t, err)
	require.NotNil(t, gotJob2)
	assert.Equal(t, job2, gotJob2)

	gotJob3, err := p.GetByJID(ctx, job3.ClientID, job3.JID)
	require.NoError(t, err)
	require.NotNil(t, gotJob3)
	assert.Equal(t, job3, gotJob3)

	// verify not found job
	gotJob4, err := p.GetByJID(ctx, job3.ClientID, "unknown-jid")
	require.NoError(t, err)
	require.Nil(t, gotJob4)

	// verify job summaries
	gotJSc1, err := p.List(ctx, &query.ListOptions{Filters: []query.FilterOption{{Column: []string{"client_id"}, Values: []string{job1.ClientID}}}})
	require.NoError(t, err)
	assert.ElementsMatch(t, []*models.Job{job1, job2}, gotJSc1)

	gotJSc2, err := p.List(ctx, &query.ListOptions{Filters: []query.FilterOption{{Column: []string{"client_id"}, Values: []string{job3.ClientID}}}})
	require.NoError(t, err)
	assert.ElementsMatch(t, []*models.Job{job3}, gotJSc2)

	// verify job summaries not found
	gotJSc3, err := p.List(ctx, &query.ListOptions{Filters: []query.FilterOption{{Column: []string{"client_id"}, Values: []string{"unknown-cid"}}}})
	require.NoError(t, err)
	require.Empty(t, gotJSc3)

	// verify job update
	job1.Status = models.JobStatusSuccessful
	job1.Result = &models.JobResult{
		StdOut: "some std out",
		StdErr: "some std err",
	}
	ft := time.Date(2020, 11, 5, 12, 11, 20, 0, time.UTC)
	job1.FinishedAt = &ft

	require.NoError(t, p.SaveJob(ctx, job1))
	gotJob1, err = p.GetByJID(ctx, job1.ClientID, job1.JID)
	require.NoError(t, err)
	require.NotNil(t, gotJob1)
	assert.Equal(t, job1, gotJob1)

	gotJSc1, err = p.List(ctx, &query.ListOptions{Filters: []query.FilterOption{{Column: []string{"client_id"}, Values: []string{job1.ClientID}}}})
	require.NoError(t, err)
	assert.ElementsMatch(t, []*models.Job{job1, job2}, gotJSc1)
}

func TestCreateJob(t *testing.T) {
	ctx := ctxFor(tenant.Default)
	jobsDB, err := sqlite.New(":memory:", jobs.FS, DataSourceOptions)
	require.NoError(t, err)
	p := NewSqliteProvider(jobsDB, testLog)
	defer p.Close()

	// create job
	job := jb.New(t).Status(models.JobStatusSuccessful).Result(nil).Build()
	require.NoError(t, p.CreateJob(ctx, job))

	// try to create the same job but with different status
	updatedJob := *job
	updatedJob.Status = models.JobStatusRunning
	require.NoError(t, p.CreateJob(ctx, job))

	// verify the job contains the initial status
	gotJob, err := p.GetByJID(ctx, job.ClientID, job.JID)
	require.NoError(t, err)
	require.Equal(t, job, gotJob)
}

func TestCreateJobPatchesLibraryFieldsOfExistingJob(t *testing.T) {
	ctx := ctxFor(tenant.Default)
	jobsDB, err := sqlite.New(":memory:", jobs.FS, DataSourceOptions)
	require.NoError(t, err)
	p := NewSqliteProvider(jobsDB, testLog)
	defer p.Close()

	// a fast result saved the job first, without library fields
	job := jb.New(t).Status(models.JobStatusSuccessful).Result(&models.JobResult{Summary: "done"}).Build()
	require.NoError(t, p.CreateJob(ctx, job))

	late := *job
	late.Status = models.JobStatusRunning
	late.Result = nil
	late.LibraryEntryID, late.LibraryVersion, late.OS = "e1", 3, "linux"
	require.NoError(t, p.CreateJob(ctx, &late))

	got, err := p.GetByJID(ctx, job.ClientID, job.JID)
	require.NoError(t, err)
	assert.Equal(t, "e1", got.LibraryEntryID)
	assert.Equal(t, 3, got.LibraryVersion)
	assert.Equal(t, "linux", got.OS)
	assert.Equal(t, models.JobStatusSuccessful, got.Status)
	assert.Equal(t, "done", got.Result.Summary)
}
