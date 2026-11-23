package schedule

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	jobsmigration "github.com/openrport/openrport/db/migration/jobs"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/server/test/jb"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/query"
)

type fakeProvider struct {
	schedule *Schedule
	inserted *Schedule
	timeout  int
	inFlight int
}

func (f *fakeProvider) Insert(_ context.Context, s *Schedule) error { f.inserted = s; return nil }
func (f *fakeProvider) Update(context.Context, *Schedule) error     { return nil }
func (f *fakeProvider) List(context.Context, *query.ListOptions) ([]*Schedule, error) {
	return nil, nil
}
func (f *fakeProvider) Get(context.Context, string) (*Schedule, error) { return f.schedule, nil }
func (f *fakeProvider) Delete(context.Context, string) error           { return nil }
func (f *fakeProvider) CountJobsInProgress(_ context.Context, _ string, timeoutSec int) (int, error) {
	f.timeout = timeoutSec
	return f.inFlight, nil
}

type fakeRunner struct {
	entryID  string
	req      *jobs.MultiJobRequest
	typed    int
	startErr error
}

func (f *fakeRunner) StartMultiClientJob(context.Context, *jobs.MultiJobRequest) (*models.MultiJob, error) {
	f.typed++
	return &models.MultiJob{}, nil
}
func (f *fakeRunner) StartLibraryMultiJob(_ context.Context, id string, r *jobs.MultiJobRequest) (*models.MultiJob, error) {
	f.entryID, f.req = id, r
	return &models.MultiJob{}, f.startErr
}

type fakeEntries struct{ entry *library.Entry }

func (f fakeEntries) ActiveForRun(_ context.Context, id string) (*library.Entry, error) {
	if f.entry == nil || f.entry.ID != id {
		return nil, errors.New("not found")
	}
	return f.entry, nil
}

type fakeSaver struct{ saved []*models.MultiJob }

func (f *fakeSaver) SaveMultiJob(mj *models.MultiJob) error {
	f.saved = append(f.saved, mj)
	return nil
}

func testEntry() *library.Entry {
	return &library.Entry{
		ID:            "e1",
		ActiveVersion: 3,
		Implementations: map[string]library.Implementation{
			"linux":   {Code: "true", TimeoutSec: 60},
			"windows": {Code: "exit", TimeoutSec: 120},
		},
	}
}

func libSchedule() *Schedule {
	return &Schedule{
		Base:    Base{ID: "s1", Type: TypeLibrary, Schedule: "* * * * *", CreatedBy: "bob"},
		Details: Details{ClientIDs: []string{"c1"}, LibraryEntryID: "e1"},
	}
}

func newLibManager(p *fakeProvider, r *fakeRunner, e *library.Entry, s *fakeSaver) *Manager {
	m := &Manager{
		Logger:                 logger.NewLogger("t", logger.LogOutput{}, logger.LogLevelError),
		jobRunner:              r,
		provider:               p,
		cron:                   newCron(),
		runRemoteCmdTimeoutSec: 30,
	}
	m.SetLibrary(fakeEntries{e}, s)
	return m
}

func TestCreateLibraryScheduleValidatesEntry(t *testing.T) {
	p := &fakeProvider{}
	m := newLibManager(p, &fakeRunner{}, testEntry(), &fakeSaver{})

	s := libSchedule()
	s.Details.Command, s.Details.Interpreter, s.Details.TimeoutSec, s.Details.IsSudo = "ls", "bash", 5, true
	got, err := m.Create(context.Background(), s, "bob")
	require.NoError(t, err)
	assert.Equal(t, "e1", got.Details.LibraryEntryID)
	assert.Empty(t, got.Details.Command)
	assert.Empty(t, got.Details.Interpreter)
	assert.Zero(t, got.Details.TimeoutSec)
	assert.False(t, got.Details.IsSudo)

	s = libSchedule()
	s.Details.LibraryEntryID = "missing"
	_, err = m.Create(context.Background(), s, "bob")
	assert.Error(t, err)

	s = libSchedule()
	s.Details.LibraryEntryID = ""
	_, err = m.Create(context.Background(), s, "bob")
	assert.Error(t, err)

	s = &Schedule{
		Base:    Base{Type: TypeCommand, Schedule: "* * * * *"},
		Details: Details{ClientIDs: []string{"c1"}, Command: "ls", LibraryEntryID: "e1"},
	}
	_, err = m.Create(context.Background(), s, "bob")
	assert.Error(t, err)
}

func TestLibraryScheduleRunsEntry(t *testing.T) {
	p := &fakeProvider{schedule: libSchedule()}
	r := &fakeRunner{}
	m := newLibManager(p, r, testEntry(), &fakeSaver{})

	m.run(context.Background(), "s1")

	assert.Equal(t, "e1", r.entryID)
	assert.Equal(t, 0, r.typed)
	assert.Equal(t, "s1", *r.req.ScheduleID)
	assert.Equal(t, []string{"c1"}, r.req.ClientIDs)
}

func TestLibraryScheduleOverlapUsesMaxTimeout(t *testing.T) {
	p := &fakeProvider{schedule: libSchedule(), inFlight: 1}
	r := &fakeRunner{}
	m := newLibManager(p, r, testEntry(), &fakeSaver{})

	m.run(context.Background(), "s1")

	assert.Equal(t, 120, p.timeout)
	assert.Empty(t, r.entryID, "overlapping run must be skipped")
}

func TestLibraryScheduleDeletedEntryRecordsError(t *testing.T) {
	p := &fakeProvider{schedule: libSchedule()}
	r := &fakeRunner{}
	saver := &fakeSaver{}
	m := newLibManager(p, r, nil, saver)

	m.run(context.Background(), "s1")

	assert.Empty(t, r.entryID)
	require.Len(t, saver.saved, 1)
	assert.Contains(t, saver.saved[0].Error, "not found")
	assert.Equal(t, "s1", *saver.saved[0].ScheduleID)
	assert.Equal(t, "e1", saver.saved[0].LibraryEntryID)

	// a start failure (e.g. no target with an implementation) is recorded too
	saver.saved = nil
	r.startErr = errors.New("no clients for execution")
	m = newLibManager(p, r, testEntry(), saver)
	m.run(context.Background(), "s1")
	require.Len(t, saver.saved, 1)
	assert.Equal(t, "no clients for execution", saver.saved[0].Error)
	assert.Equal(t, 3, saver.saved[0].LibraryVersion)
}

func lastExecution(t *testing.T, scheduleID string, build func(jp *jobs.SqliteProvider)) *Execution {
	t.Helper()
	db, err := sqlite.New(":memory:", jobsmigration.AssetNames(), jobsmigration.Asset, DataSourceOptions)
	require.NoError(t, err)
	prov := newSQLiteProvider(db)
	defer prov.Close()
	require.NoError(t, addTestData(db))
	build(jobs.NewSqliteProvider(db, logger.NewLogger("t", logger.LogOutput{}, logger.LogLevelError)))
	s, err := prov.Get(context.Background(), scheduleID)
	require.NoError(t, err)
	return s.LastExecution
}

func TestLastExecutionOfFailedLibraryRun(t *testing.T) {
	ex := lastExecution(t, "1", func(jp *jobs.SqliteProvider) {
		mj := jb.NewMulti(t).ScheduleID("1").Build()
		mj.Error = "entry deleted"
		require.NoError(t, jp.SaveMultiJob(mj))
	})
	require.NotNil(t, ex)
	assert.Equal(t, "failed", *ex.Status)
	assert.Equal(t, "entry deleted", *ex.Error)
	assert.Equal(t, 0, *ex.ClientCount)
}

func TestLastExecutionExcludesSkippedJobs(t *testing.T) {
	ex := lastExecution(t, "1", func(jp *jobs.SqliteProvider) {
		mj := jb.NewMulti(t).ScheduleID("1").Build()
		require.NoError(t, jp.SaveMultiJob(mj))
		for _, st := range []string{"successful", "skipped", "skipped"} {
			require.NoError(t, jp.SaveJob(jb.New(t).MultiJobID(mj.JID).Status(st).Build()))
		}
	})
	require.NotNil(t, ex)
	assert.Equal(t, 1, *ex.ClientCount)
	assert.Equal(t, 1, *ex.SuccessCount)
	assert.Nil(t, ex.Error)
}

func TestLastExecutionTypedUnchanged(t *testing.T) {
	ex := lastExecution(t, "1", func(jp *jobs.SqliteProvider) {
		// a multi-job without jobs and without error is not an execution
		require.NoError(t, jp.SaveMultiJob(jb.NewMulti(t).ScheduleID("1").StartedAt(time.Now().Add(time.Minute)).Build()))
		mj := jb.NewMulti(t).ScheduleID("1").Build()
		require.NoError(t, jp.SaveMultiJob(mj))
		require.NoError(t, jp.SaveJob(jb.New(t).MultiJobID(mj.JID).Status("failed").Build()))
		require.NoError(t, jp.SaveJob(jb.New(t).MultiJobID(mj.JID).Status("successful").Build()))
	})
	require.NotNil(t, ex)
	assert.Equal(t, 2, *ex.ClientCount)
	assert.Equal(t, 1, *ex.SuccessCount)
	assert.Nil(t, ex.Status, "status only for single-client runs")
	assert.Nil(t, ex.Error)
}

func TestUpdateLibrarySchedule(t *testing.T) {
	p := &fakeProvider{schedule: libSchedule()}
	m := newLibManager(p, &fakeRunner{}, testEntry(), &fakeSaver{})

	s := libSchedule()
	s.Details.LibraryEntryID = "missing"
	_, err := m.Update(context.Background(), "s1", s)
	assert.Error(t, err)

	s = libSchedule()
	s.Schedule = "bad"
	_, err = m.Update(context.Background(), "s1", s)
	assert.Error(t, err)

	s = libSchedule()
	s.Details.Command = "ls"
	got, err := m.Update(context.Background(), "s1", s)
	require.NoError(t, err)
	assert.Equal(t, "e1", got.Details.LibraryEntryID)
	assert.Equal(t, "s1", s.ID)
	assert.Empty(t, s.Details.Command)
}
