package chserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/server/clients/clienttunnel"
	"github.com/openrport/openrport/server/dispatch"
	"github.com/openrport/openrport/share/comm"
	"github.com/openrport/openrport/share/files"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/tenant"
)

// recordingDispatcher records the jobs it is asked to run and starts every one with pid 7.
type recordingDispatcher struct {
	mu   sync.Mutex
	jobs map[string][]*models.Job // by client ID
	ids  []string                 // client IDs in call order
}

func (d *recordingDispatcher) RunJob(_ context.Context, clientID string, job *models.Job) (*comm.RunCmdResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.jobs == nil {
		d.jobs = map[string][]*models.Job{}
	}
	d.jobs[clientID] = append(d.jobs[clientID], job)
	d.ids = append(d.ids, clientID)
	return &comm.RunCmdResponse{Pid: 7}, nil
}

func (d *recordingDispatcher) clientIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.ids...)
}

func (d *recordingDispatcher) Upload(context.Context, string, *models.UploadedFile) (*models.UploadResponse, error) {
	return &models.UploadResponse{}, nil
}
func (d *recordingDispatcher) IsTunnelAllowed(context.Context, string, string) (bool, error) {
	return true, nil
}
func (d *recordingDispatcher) CheckPort(context.Context, string, *comm.CheckPortRequest) (*comm.CheckPortResponse, error) {
	return &comm.CheckPortResponse{Open: true}, nil
}
func (d *recordingDispatcher) StartTunnels(context.Context, string, []*models.Remote) ([]*clienttunnel.Tunnel, error) {
	return nil, nil
}
func (d *recordingDispatcher) CloseTunnel(context.Context, string, string, bool) error { return nil }
func (d *recordingDispatcher) SetTunnelACL(context.Context, string, string, *string) error {
	return nil
}
func (d *recordingDispatcher) RefreshUpdatesStatus(context.Context, string) error { return nil }
func (d *recordingDispatcher) UpdateAttributes(context.Context, string, models.Attributes) error {
	return nil
}

func TestPostCommandUsesDispatcher(t *testing.T) {
	c1 := clients.New(t).ID("c1").Logger(testLog).Build()
	d := &recordingDispatcher{}
	jp := NewJobProviderMock()
	user := &users.User{Username: "admin", Groups: []string{users.Administrators}}
	al := &APIListener{
		insecureForTests: true,
		apiServices: &apiServices{
			clientService: clients.NewClientService(nil, nil, clients.NewClientRepository([]*clientdata.Client{c1}, &hour, testLog), testLog, nil),
			config:        &chconfig.Config{API: chconfig.APIConfig{MaxRequestBytes: 1 << 20}},
			jobProvider:   jp,
			dispatcher:    d,
		},
		userService: users.NewAPIService(users.NewStaticProvider([]*users.User{user}), false, 0, -1),
		Logger:      testLog,
	}
	al.initRouter()

	ctx := api.WithUser(tenant.WithID(context.Background(), tenant.Default), "admin")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/clients/c1/commands", strings.NewReader(`{"command":"date"}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	al.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `{"data":{"jid":"`)
	require.Len(t, d.jobs["c1"], 1)
	assert.Equal(t, "date", d.jobs["c1"][0].Command)
	require.NotNil(t, jp.InputCreateJob)
	assert.Equal(t, 7, *jp.InputCreateJob.PID, "the dispatcher's answer is saved")
}

func TestNewServerUsesInjectedDispatcher(t *testing.T) {
	d := &recordingDispatcher{}
	s, err := NewServer(context.Background(), storageTestConfig(t), &ServerOpts{FilesAPI: files.NewFileSystem(), Dispatcher: d})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	assert.Same(t, d, s.dispatcher)
	assert.Same(t, d, s.apiListener.dispatcher, "the API dispatches through it")

	// the job runner (used by the API and the schedules) dispatches through it too
	_, err = s.jobRunner.StartMultiClientJob(tenant.WithID(context.Background(), tenant.Default), &jobs.MultiJobRequest{
		ClientIDs:           []string{"x"},
		Command:             "date",
		ExecuteConcurrently: true,
		OrderedClients:      []*clientdata.Client{{ID: "x"}},
	})
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return len(d.clientIDs()) == 1 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"x"}, d.clientIDs())

	local, err := NewServer(context.Background(), storageTestConfig(t), &ServerOpts{FilesAPI: files.NewFileSystem()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = local.Close() })
	assert.IsType(t, &dispatch.Local{}, local.dispatcher)
}

// NewServer gives the client listener, the job runner, the API listener and the local dispatcher
// the same shared state: a sequential run waits on the channels the listener signals, job results
// stream to the websockets the API registered, and SFTP serves the files uploads granted.
func TestNewServerSharesStateBetweenComponents(t *testing.T) {
	s, err := NewServer(context.Background(), storageTestConfig(t), &ServerOpts{FilesAPI: files.NewFileSystem()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	listener := s.clientListener.server
	assert.Same(t, s.jobRunner.DoneChannels(), listener.jobsDoneChannel, "done channels")
	assert.Same(t, &s.uiJobWebSockets, listener.uiJobWebSockets, "UI job streams (client listener)")
	assert.Same(t, &s.uiJobWebSockets, s.apiListener.uiJobWebSockets, "UI job streams (API)")
	local, ok := s.dispatcher.(*dispatch.Local)
	require.True(t, ok)
	assert.Same(t, local.FileGrants(), listener.fileGrants, "file grants")
}

// A dispatcher that starts no tunnel without an error gets a 500, not a crash.
func TestPutTunnelWithoutStartedTunnel(t *testing.T) {
	c1 := clients.New(t).ID("c1").Logger(testLog).Build()
	user := &users.User{Username: "admin", Groups: []string{users.Administrators}}
	al := &APIListener{
		insecureForTests: true,
		apiServices: &apiServices{
			clientService: clients.NewClientService(nil, nil, clients.NewClientRepository([]*clientdata.Client{c1}, &hour, testLog), testLog, nil),
			config:        &chconfig.Config{API: chconfig.APIConfig{MaxRequestBytes: 1 << 20}},
			dispatcher:    &recordingDispatcher{},
		},
		userService: users.NewAPIService(users.NewStaticProvider([]*users.User{user}), false, 0, -1),
		Logger:      testLog,
	}
	al.initRouter()

	ctx := api.WithUser(tenant.WithID(context.Background(), tenant.Default), "admin")
	req := httptest.NewRequest(http.MethodPut, "/api/v1/clients/c1/tunnels?remote=127.0.0.1%3A22&check_port=0", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	al.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "expected 1 started tunnel, got 0")
}
