package schedule

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	jobsmigration "github.com/pohcee/procy-server/db/migration/jobs"
	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/server/api/jobs"
	"github.com/pohcee/procy-server/server/test/jb"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/models"
	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/tenant"
)

func ctxFor(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

func newTenantTestDB(t *testing.T) (*SQLiteProvider, *jobs.SqliteProvider) {
	t.Helper()
	db, err := sqlite.New(":memory:", jobsmigration.FS, DataSourceOptions)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return NewSQLiteProvider(db), jobs.NewSqliteProvider(db, logger.NewLogger("t", logger.LogOutput{}, logger.LogLevelError))
}

func namedSchedule(id, name string) *Schedule {
	s := *testData[0]
	s.ID, s.Name = id, name
	return &s
}

func TestScheduleProviderRequiresTenant(t *testing.T) {
	p, _ := newTenantTestDB(t)
	ctx := context.Background()
	assert.ErrorIs(t, p.Insert(ctx, namedSchedule("s1", "n")), tenant.ErrMissing)
	assert.ErrorIs(t, p.Update(ctx, namedSchedule("s1", "n")), tenant.ErrMissing)
	_, err := p.List(ctx, nil)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.Get(ctx, "s1")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.ErrorIs(t, p.Delete(ctx, "s1"), tenant.ErrMissing)
	_, err = p.CountJobsInProgress(ctx, "s1", 60)
	assert.ErrorIs(t, err, tenant.ErrMissing)
}

func TestScheduleProviderIsolatesTenants(t *testing.T) {
	p, jp := newTenantTestDB(t)
	a, b := ctxFor("a"), ctxFor("b")
	require.NoError(t, p.Insert(a, namedSchedule("s1", "from a")))
	require.NoError(t, p.Insert(b, namedSchedule("s1", "from b")))

	// the same multi-job and job IDs in both tenants; only b's job is still running
	for _, c := range []struct {
		ctx    context.Context
		status string
	}{{a, models.JobStatusSuccessful}, {b, models.JobStatusRunning}} {
		require.NoError(t, jp.SaveMultiJob(c.ctx, jb.NewMulti(t).JID("mj1").ScheduleID("s1").Build()))
		job := jb.New(t).JID("j1").MultiJobID("mj1").Status(c.status).StartedAt(time.Now()).Build()
		if c.status == models.JobStatusSuccessful {
			job = jb.New(t).JID("j1").MultiJobID("mj1").Status(c.status).StartedAt(time.Now()).FinishedAt(time.Now()).Build()
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

	listed, err := p.List(b, &query.ListOptions{})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, "from b", listed[0].Name)
	got, err = p.Get(ctxFor("c"), "s1")
	require.NoError(t, err)
	assert.Nil(t, got)

	n, err := p.CountJobsInProgress(a, "s1", 60)
	require.NoError(t, err)
	assert.Zero(t, n)
	n, err = p.CountJobsInProgress(b, "s1", 60)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	require.NoError(t, p.Update(a, namedSchedule("s1", "a updated")))
	got, err = p.Get(b, "s1")
	require.NoError(t, err)
	assert.Equal(t, "from b", got.Name)

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
	assert.Error(t, p.Delete(ctxFor("c"), "s1"))
}

func TestScheduleJSONHasNoTenant(t *testing.T) {
	p, _ := newTenantTestDB(t)
	ctx := ctxFor("a")
	require.NoError(t, p.Insert(ctx, namedSchedule("s1", "n")))
	got, err := p.Get(ctx, "s1")
	require.NoError(t, err)
	b, err := json.Marshal(got)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "tenant")
}

// Cron entries are keyed by tenant and schedule ID, and each runs with its own tenant on top of the
// cron's base context.
func TestCronEntriesRunWithTheirTenant(t *testing.T) {
	type baseKey struct{}
	c := newCron(context.WithValue(tenant.WithID(context.Background(), tenant.Default), baseKey{}, "base"))
	got := make(chan tenant.ID, 10)
	f := func(ctx context.Context, _ string) {
		if ctx.Value(baseKey{}) != "base" {
			return
		}
		id, _ := tenant.FromContext(ctx)
		select {
		case got <- id:
		default:
		}
	}
	require.NoError(t, c.Add(ctxFor("a"), "s1", "@every 1s", f))
	require.NoError(t, c.Add(ctxFor("b"), "s1", "@every 1s", f))
	c.Remove(ctxFor("a"), "s1")
	defer c.Remove(ctxFor("b"), "s1")

	select {
	case id := <-got:
		assert.Equal(t, tenant.ID("b"), id)
	case <-time.After(3 * time.Second):
		t.Fatal("cron callback did not run")
	}
	c.mtx.Lock()
	assert.Len(t, c.mapping, 1)
	c.mtx.Unlock()

	assert.ErrorIs(t, c.Add(context.Background(), "s2", "@every 1s", f), tenant.ErrMissing)
}

type ctxRunner struct {
	fakeRunner
	tenants chan tenant.ID
}

func (r *ctxRunner) StartMultiClientJob(ctx context.Context, _ *jobs.MultiJobRequest) (*models.MultiJob, error) {
	id, _ := tenant.FromContext(ctx)
	select {
	case r.tenants <- id:
	default:
	}
	return &models.MultiJob{}, nil
}

// A schedule created in one tenant fires with that tenant, not the manager's.
func TestScheduleFiresWithItsTenant(t *testing.T) {
	sp, _ := newTenantTestDB(t)
	r := &ctxRunner{tenants: make(chan tenant.ID, 10)}
	m := &Manager{
		Logger:                 logger.NewLogger("t", logger.LogOutput{}, logger.LogLevelError),
		jobRunner:              r,
		provider:               sp,
		cron:                   newCron(ctxFor(tenant.Default)),
		runRemoteCmdTimeoutSec: 30,
	}
	a := ctxFor("a")
	s, err := m.Create(a, &Schedule{
		Base:    Base{Name: "n", Schedule: "@every 1s", Type: TypeCommand},
		Details: Details{ClientIDs: []string{"c1"}, Command: "ls", Overlaps: true},
	}, "bob")
	require.NoError(t, err)
	defer func() { _ = m.Delete(a, s.ID) }()

	select {
	case id := <-r.tenants:
		assert.Equal(t, tenant.ID("a"), id)
	case <-time.After(3 * time.Second):
		t.Fatal("schedule did not fire")
	}
}

type ctxSaver struct{ tenants []tenant.ID }

func (s *ctxSaver) SaveMultiJob(ctx context.Context, _ *models.MultiJob) error {
	id, err := tenant.FromContext(ctx)
	if err != nil {
		return err
	}
	s.tenants = append(s.tenants, id)
	return nil
}

// A library run that fails to start is recorded in the tenant of its run.
func TestRecordFailureUsesRunTenant(t *testing.T) {
	p := &fakeProvider{schedule: libSchedule()}
	saver := &ctxSaver{}
	m := &Manager{
		Logger:                 logger.NewLogger("t", logger.LogOutput{}, logger.LogLevelError),
		jobRunner:              &fakeRunner{},
		provider:               p,
		cron:                   newCron(ctxFor(tenant.Default)),
		runRemoteCmdTimeoutSec: 30,
	}
	m.SetLibrary(fakeEntries{nil}, saver)
	m.run(ctxFor("a"), "s1")
	assert.Equal(t, []tenant.ID{"a"}, saver.tenants)
}
