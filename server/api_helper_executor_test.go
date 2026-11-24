package chserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/server/jobrunner"
	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/tenant"
	"github.com/openrport/openrport/share/test"
	"github.com/openrport/openrport/share/ws"
)

// fakeUIConn records what the executor writes to the UI websocket.
type fakeUIConn struct {
	mu     sync.Mutex
	writes [][]byte
	closed bool
}

func (c *fakeUIConn) NextReader() (int, io.Reader, error) { return 0, nil, errors.New("no reads") }
func (c *fakeUIConn) ReadMessage() (int, []byte, error)   { return 0, nil, errors.New("closed") }
func (c *fakeUIConn) WriteMessage(_ int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, data)
	return nil
}
func (c *fakeUIConn) WriteJSON(v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.WriteMessage(0, b)
}
func (c *fakeUIConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
func (c *fakeUIConn) messages(t *testing.T) []map[string]interface{} {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	res := make([]map[string]interface{}, 0, len(c.writes))
	for _, w := range c.writes {
		m := map[string]interface{}{}
		require.NoError(t, json.Unmarshal(w, &m))
		res = append(res, m)
	}
	return res
}

// sequentialJobIDs makes generateNewJobID deterministic for the test.
func sequentialJobIDs(t *testing.T) {
	t.Helper()
	orig := generateNewJobID
	var mu sync.Mutex
	n := 0
	generateNewJobID = func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		return fmt.Sprintf("jid-%d", n), nil
	}
	t.Cleanup(func() { generateNewJobID = orig })
}

func rawJobDetails(t *testing.T, jp *jobs.SqliteProvider, jid string) string {
	t.Helper()
	var details string
	require.NoError(t, jobsDBOf(t, jp).Get(&details, "SELECT details FROM jobs WHERE jid=?", jid))
	return details
}

func rawMultiJobDetails(t *testing.T, jp *jobs.SqliteProvider, jid string) string {
	t.Helper()
	var details string
	require.NoError(t, jobsDBOf(t, jp).Get(&details, "SELECT details FROM multi_jobs WHERE jid=?", jid))
	return details
}

// sentJob decodes the RunCmd payload a client received, dropping started_at (time.Now()).
func sentJob(t *testing.T, conn *test.ConnMock) map[string]interface{} {
	t.Helper()
	_, _, payload := conn.InputSendRequest()
	require.NotEmpty(t, payload)
	m := map[string]interface{}{}
	require.NoError(t, json.Unmarshal(payload, &m))
	delete(m, "started_at")
	return m
}

func jobStatuses(mj *models.MultiJob) map[string]string {
	res := map[string]string{}
	for _, j := range mj.Jobs {
		res[j.JID] = j.Status
	}
	return res
}

func typedRunFixture(t *testing.T) (*APIListener, *jobs.SqliteProvider, []*clientdata.Client, []*test.ConnMock) {
	t.Helper()
	conn1 := makeConnMock(t, 1, time.Date(2020, 10, 10, 10, 10, 1, 0, time.UTC))
	conn3 := makeConnMock(t, 3, time.Date(2020, 10, 10, 10, 10, 3, 0, time.UTC))
	c1 := clients.New(t).ID("c1").Connection(conn1).Logger(testLog).Build()
	c2 := clients.New(t).ID("c2").Connection(test.NewConnMock()).Logger(testLog).Build()
	c2.SetPaused(true, "maintenance")
	c3 := clients.New(t).ID("c3").Connection(conn3).Logger(testLog).Build()
	cls := []*clientdata.Client{c1, c2, c3}

	al := makeAPIListener(makeTestUser("tester"),
		clients.NewClientRepository(cls, &hour, testLog), 60, nil, testLog)
	al.clientGroupProvider = mockClientGroupProvider{}
	jp := makeJobsProvider(t, DataSourceOptions, testLog)
	t.Cleanup(func() { jp.Close() })
	al.jobProvider = jp
	return al, jp, cls, []*test.ConnMock{conn1, conn3}
}

const (
	wantTypedJobDetailsRunning = `{"command":"uname -a","cwd":"/tmp","is_sudo":true,"is_script":false,"interpreter":"","pid":%d,"timeout_sec":30,"error":"","result":null,"client_name":"Random Rport Client"}`
	wantTypedJobDetailsPaused  = `{"command":"uname -a","cwd":"/tmp","is_sudo":true,"is_script":false,"interpreter":"","pid":null,"timeout_sec":30,"error":"client is paused (reason = maintenance)","result":null,"client_name":"Random Rport Client"}`
	wantTypedMultiJobDetails   = `{"client_ids":["c1","c2","c3"],"group_ids":null,"tags":null,"command":"uname -a","interpreter":"","cwd":"/tmp","is_sudo":true,"timeout_sec":30,"concurrent":false,"abort_on_err":false}`
)

func wantTypedSentJob(jid, clientID string, streamResult bool) map[string]interface{} {
	return map[string]interface{}{
		"jid": jid, "status": "", "finished_at": nil, "client_id": clientID,
		"client_name": "Random Rport Client", "command": "uname -a", "cwd": "/tmp",
		"interpreter": "", "pid": nil, "created_by": "tester", "timeout_sec": float64(30),
		"multi_job_id": "jid-1", "schedule_id": nil, "error": "", "result": nil,
		"is_sudo": true, "is_script": false, "stream_result": streamResult,
	}
}

// TestTypedRunUnchanged pins the job details, client payloads and websocket
// messages a typed command run produced before the executor refactor.
func TestTypedRunUnchanged(t *testing.T) {
	abortOnErr := false

	t.Run("api multi-job", func(t *testing.T) {
		sequentialJobIDs(t)
		al, jp, cls, conns := typedRunFixture(t)
		done := make(chan bool)
		al.testDone = done

		multiJob, err := al.jobRunner.StartMultiClientJob(tenant.WithID(context.Background(), tenant.Default), &jobs.MultiJobRequest{
			ClientIDs:      []string{"c1", "c2", "c3"},
			Command:        "uname -a",
			Cwd:            "/tmp",
			IsSudo:         true,
			TimeoutSec:     30,
			AbortOnError:   &abortOnErr,
			Username:       "tester",
			OrderedClients: cls,
		})
		require.NoError(t, err)
		require.Equal(t, "jid-1", multiJob.JID)
		<-done

		assert.JSONEq(t, wantTypedMultiJobDetails, rawMultiJobDetails(t, jp, "jid-1"))
		assert.JSONEq(t, fmt.Sprintf(wantTypedJobDetailsRunning, 1), rawJobDetails(t, jp, "jid-2"))
		assert.JSONEq(t, wantTypedJobDetailsPaused, rawJobDetails(t, jp, "jid-3"))
		assert.JSONEq(t, fmt.Sprintf(wantTypedJobDetailsRunning, 3), rawJobDetails(t, jp, "jid-4"))

		got, err := jp.GetMultiJob(tenant.WithID(context.Background(), tenant.Default), "jid-1")
		require.NoError(t, err)
		require.Len(t, got.Jobs, 3)
		assert.Equal(t, map[string]string{
			"jid-2": models.JobStatusRunning, "jid-3": models.JobStatusFailed, "jid-4": models.JobStatusRunning,
		}, jobStatuses(got))

		assert.Equal(t, wantTypedSentJob("jid-2", "c1", false), sentJob(t, conns[0]))
		assert.Equal(t, wantTypedSentJob("jid-4", "c3", false), sentJob(t, conns[1]))
	})

	t.Run("websocket multi-job", func(t *testing.T) {
		sequentialJobIDs(t)
		al, jp, cls, conns := typedRunFixture(t)
		uiConn := &fakeUIConn{}
		uiConnTS := ws.NewConcurrentWebSocket(uiConn, testLog)
		ctx := api.WithUser(tenant.WithID(context.Background(), tenant.Default), "tester")

		al.handleCommandsExecutionWS(ctx, uiConnTS, &jobs.MultiJobRequest{
			ClientIDs:      []string{"c1", "c2", "c3"},
			Command:        "uname -a",
			Cwd:            "/tmp",
			IsSudo:         true,
			TimeoutSec:     30,
			AbortOnError:   &abortOnErr,
			OrderedClients: cls,
		}, nil)

		assert.JSONEq(t, wantTypedMultiJobDetails, rawMultiJobDetails(t, jp, "jid-1"))
		assert.JSONEq(t, fmt.Sprintf(wantTypedJobDetailsRunning, 1), rawJobDetails(t, jp, "jid-2"))
		assert.JSONEq(t, wantTypedJobDetailsPaused, rawJobDetails(t, jp, "jid-3"))
		assert.JSONEq(t, fmt.Sprintf(wantTypedJobDetailsRunning, 3), rawJobDetails(t, jp, "jid-4"))

		assert.Equal(t, wantTypedSentJob("jid-2", "c1", true), sentJob(t, conns[0]))
		assert.Equal(t, wantTypedSentJob("jid-4", "c3", true), sentJob(t, conns[1]))

		// only the failed (paused) job is written by the server; running jobs
		// reach the UI from the client listener.
		msgs := uiConn.messages(t)
		require.Len(t, msgs, 1)
		require.NotNil(t, msgs[0]["finished_at"])
		delete(msgs[0], "started_at")
		delete(msgs[0], "finished_at")
		want := wantTypedSentJob("jid-3", "c2", true)
		delete(want, "finished_at")
		want["status"] = models.JobStatusFailed
		want["error"] = "client is paused (reason = maintenance)"
		assert.Equal(t, want, msgs[0])
	})

	t.Run("websocket abort on error", func(t *testing.T) {
		sequentialJobIDs(t)
		al, jp, cls, _ := typedRunFixture(t)
		done := make(chan bool, 1)
		al.testDone = done
		uiConn := &fakeUIConn{}
		uiConnTS := ws.NewConcurrentWebSocket(uiConn, testLog)
		ctx := api.WithUser(tenant.WithID(context.Background(), tenant.Default), "tester")
		abort := true

		al.handleCommandsExecutionWS(ctx, uiConnTS, &jobs.MultiJobRequest{
			ClientIDs:      []string{"c2", "c1"},
			Command:        "uname -a",
			TimeoutSec:     30,
			AbortOnError:   &abort,
			OrderedClients: []*clientdata.Client{cls[1], cls[0]},
		}, nil)

		got, err := jp.GetMultiJob(tenant.WithID(context.Background(), tenant.Default), "jid-1")
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"jid-2": models.JobStatusFailed}, jobStatuses(got))
		assert.True(t, uiConn.closed)
		assert.Len(t, done, 0, "aborted websocket run does not signal testDone")
		assert.Len(t, uiConn.messages(t), 1)
	})

	t.Run("api abort on error", func(t *testing.T) {
		sequentialJobIDs(t)
		al, jp, cls, _ := typedRunFixture(t)
		done := make(chan bool, 1)
		al.testDone = done
		abort := true

		_, err := al.jobRunner.StartMultiClientJob(tenant.WithID(context.Background(), tenant.Default), &jobs.MultiJobRequest{
			ClientIDs:      []string{"c2", "c1"},
			Command:        "uname -a",
			TimeoutSec:     30,
			AbortOnError:   &abort,
			Username:       "tester",
			OrderedClients: []*clientdata.Client{cls[1], cls[0]},
		})
		require.NoError(t, err)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("api run did not finish")
		}
		got, err := jp.GetMultiJob(tenant.WithID(context.Background(), tenant.Default), "jid-1")
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"jid-2": models.JobStatusFailed}, jobStatuses(got))
	})

}

// doneSignalingJobProvider reports every running multi-job job as finished
// successfully, standing in for the client listener in sequential runs.
type doneSignalingJobProvider struct {
	*jobs.SqliteProvider
	al *APIListener
}

func (p *doneSignalingJobProvider) CreateJob(ctx context.Context, job *models.Job) error {
	err := p.SqliteProvider.CreateJob(ctx, job)
	if job.Status == models.JobStatusRunning && job.MultiJobID != nil {
		if ch := p.al.jobsDoneChannel.Get(*job.MultiJobID); ch != nil {
			finished := *job
			finished.Status = models.JobStatusSuccessful
			go func() { ch <- &finished }()
		}
	}
	return err
}

func executorFixture(t *testing.T) (*APIListener, *jobs.SqliteProvider, []*clientdata.Client, []*test.ConnMock) {
	t.Helper()
	conns := []*test.ConnMock{
		makeConnMock(t, 1, time.Date(2020, 10, 10, 10, 10, 1, 0, time.UTC)),
		makeConnMock(t, 2, time.Date(2020, 10, 10, 10, 10, 2, 0, time.UTC)),
		makeConnMock(t, 3, time.Date(2020, 10, 10, 10, 10, 3, 0, time.UTC)),
	}
	cls := []*clientdata.Client{
		clients.New(t).ID("c1").Connection(conns[0]).Logger(testLog).Build(),
		clients.New(t).ID("c2").Connection(conns[1]).Logger(testLog).Build(),
		clients.New(t).ID("c3").Connection(conns[2]).Logger(testLog).Build(),
	}
	cls[1].OSKernel = "windows"

	al := makeAPIListener(makeTestUser("tester"),
		clients.NewClientRepository(cls, &hour, testLog), 60, nil, testLog)
	al.insecureForTests = false // really wait for each sequential job
	useLocalDispatcher(al)
	jp := makeJobsProvider(t, DataSourceOptions, testLog)
	t.Cleanup(func() { jp.Close() })
	al.jobProvider = &doneSignalingJobProvider{SqliteProvider: jp, al: al}
	return al, jp, cls, conns
}

// linuxOnlyEntry is a specFor for an entry with only a Linux implementation.
func linuxOnlyEntry(c *clientdata.Client) (library.JobSpec, string) {
	osKernel := c.GetOSKernel()
	if osKernel != library.OSLinux {
		return library.JobSpec{LibraryEntryID: "entry-1", LibraryVersion: 3, OS: osKernel},
			"no " + library.OSLabel(osKernel) + " implementation"
	}
	return library.JobSpec{
		Command: "uptime", Interpreter: "/bin/bash", Cwd: "/root", TimeoutSec: 45, IsSudo: true,
		LibraryEntryID: "entry-1", LibraryVersion: 3, OS: library.OSLinux,
	}, ""
}

func runExecutor(t *testing.T, al *APIListener, uiConn *ws.ConcurrentWebSocket, mj *models.MultiJob, cls []*clientdata.Client, specFor jobrunner.ClientSpecFunc) {
	t.Helper()
	require.NoError(t, al.jobProvider.SaveMultiJob(tenant.WithID(context.Background(), tenant.Default), mj))
	finished := make(chan struct{})
	go func() {
		al.jobRunner.RunMultiClientJob(tenant.WithID(context.Background(), tenant.Default), uiConn, mj, cls, specFor)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not finish (blocked waiting on a skipped job?)")
	}
}

func jobsByClient(t *testing.T, jp *jobs.SqliteProvider, multiJobID string) map[string]*models.Job {
	t.Helper()
	mj, err := jp.GetMultiJob(tenant.WithID(context.Background(), tenant.Default), multiJobID)
	require.NoError(t, err)
	res := map[string]*models.Job{}
	for _, j := range mj.Jobs {
		res[j.ClientID] = j
	}
	return res
}

func TestExecutorSkipDoesNotAbort(t *testing.T) {
	sequentialJobIDs(t)
	al, jp, cls, conns := executorFixture(t)
	mj := &models.MultiJob{
		MultiJobSummary: models.MultiJobSummary{JID: "multi-1", StartedAt: time.Now(), CreatedBy: "tester"},
		AbortOnErr:      true,
		Concurrent:      false,
	}

	runExecutor(t, al, nil, mj, cls, linuxOnlyEntry)

	got := jobsByClient(t, jp, "multi-1")
	require.Len(t, got, 3)
	assert.Equal(t, models.JobStatusRunning, got["c1"].Status)
	assert.Equal(t, models.JobStatusRunning, got["c3"].Status, "a skipped client must not abort the run")

	skipped := got["c2"]
	assert.Equal(t, models.JobStatusSkipped, skipped.Status)
	require.NotNil(t, skipped.Result)
	assert.Equal(t, "no Windows implementation", skipped.Result.Summary)
	require.NotNil(t, skipped.FinishedAt)
	assert.Equal(t, "tester", skipped.CreatedBy)
	assert.Equal(t, "Random Rport Client", skipped.ClientName)

	_, _, payload := conns[1].InputSendRequest()
	assert.Empty(t, payload, "a skipped client is never sent the job")
	_, _, payload = conns[2].InputSendRequest()
	assert.NotEmpty(t, payload)
}

func TestExecutorSkipWritesToWebSocket(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(fmt.Sprintf("concurrent=%v", concurrent), func(t *testing.T) {
			sequentialJobIDs(t)
			al, jp, cls, _ := executorFixture(t)
			uiConn := &fakeUIConn{}
			uiConnTS := ws.NewConcurrentWebSocket(uiConn, testLog)
			uiConnTS.SetWritesBeforeClose(len(cls))
			mj := &models.MultiJob{
				MultiJobSummary: models.MultiJobSummary{JID: "multi-1", StartedAt: time.Now(), CreatedBy: "tester"},
				AbortOnErr:      true,
				Concurrent:      concurrent,
			}

			runExecutor(t, al, uiConnTS, mj, cls, linuxOnlyEntry)
			if concurrent {
				require.Eventually(t, func() bool { return len(jobsByClient(t, jp, "multi-1")) == 3 }, 5*time.Second, 10*time.Millisecond)
			}

			msgs := uiConn.messages(t)
			require.Len(t, msgs, 1)
			assert.Equal(t, models.JobStatusSkipped, msgs[0]["status"])
			assert.Equal(t, "c2", msgs[0]["client_id"])
			assert.Equal(t, "multi-1", msgs[0]["multi_job_id"])
			assert.Equal(t, map[string]interface{}{"stdout": "", "stderr": "", "summary": "no Windows implementation"}, msgs[0]["result"])
			assert.Equal(t, "entry-1", msgs[0]["library_entry_id"])
			assert.Equal(t, "windows", msgs[0]["os"])
			assert.False(t, uiConn.closed, "skip counts as one of the expected writes, not the last")
		})
	}
}

func TestExecutorRecordsLibraryFields(t *testing.T) {
	sequentialJobIDs(t)
	al, jp, cls, conns := executorFixture(t)
	mj := &models.MultiJob{
		MultiJobSummary: models.MultiJobSummary{JID: "multi-1", StartedAt: time.Now(), CreatedBy: "tester"},
		Concurrent:      false,
		LibraryEntryID:  "entry-1",
		LibraryVersion:  3,
	}

	runExecutor(t, al, nil, mj, cls, linuxOnlyEntry)

	gotMJ, err := jp.GetMultiJob(tenant.WithID(context.Background(), tenant.Default), "multi-1")
	require.NoError(t, err)
	assert.Equal(t, "entry-1", gotMJ.LibraryEntryID)
	assert.Equal(t, 3, gotMJ.LibraryVersion)

	got := jobsByClient(t, jp, "multi-1")
	require.Len(t, got, 3)
	for id, wantOS := range map[string]string{"c1": "linux", "c2": "windows", "c3": "linux"} {
		assert.Equal(t, "entry-1", got[id].LibraryEntryID, id)
		assert.Equal(t, 3, got[id].LibraryVersion, id)
		assert.Equal(t, wantOS, got[id].OS, id)
	}
	ran := got["c1"]
	assert.Equal(t, "uptime", ran.Command)
	assert.Equal(t, "/bin/bash", ran.Interpreter)
	assert.Equal(t, "/root", ran.Cwd)
	assert.Equal(t, 45, ran.TimeoutSec)
	assert.True(t, ran.IsSudo)

	sent := sentJob(t, conns[0])
	assert.Equal(t, "uptime", sent["command"])
	assert.Equal(t, "entry-1", sent["library_entry_id"])
	assert.Equal(t, "linux", sent["os"])
}
