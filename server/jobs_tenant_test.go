package chserver

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/share/comm"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
	"github.com/openrport/openrport/share/ws"
)

// A job result sent by a device is saved in the tenant of the device's client context.
func TestCmdResultSavedInClientTenant(t *testing.T) {
	jp := makeJobsProvider(t, DataSourceOptions, testLog)
	t.Cleanup(func() { jp.Close() })
	cl := &ClientListener{logger: testLog, server: &Server{
		config:          &chconfig.Config{Server: chconfig.ServerConfig{MaxRequestBytesClient: 1024 * 1024}},
		jobProvider:     jp,
		uiJobWebSockets: ws.NewWebSocketCache(),
	}}

	reqs := make(chan *ssh.Request, 1)
	reqs <- &ssh.Request{Type: comm.RequestTypeCmdResult, Payload: []byte(`{"jid":"j1","client_id":"c1","status":"successful","started_at":"2026-01-01T00:00:00Z"}`)}
	close(reqs)
	clientLog := logger.NewDynamicLogger("test", logger.LogOutput{}, logger.LogLevelError, false, false)
	cl.handleSSHRequests(tenant.WithID(context.Background(), "a"), clientLog, "c1", reqs)

	got, err := jp.GetByJID(tenant.WithID(context.Background(), "a"), "c1", "j1")
	require.NoError(t, err)
	require.NotNil(t, got, "result is saved in the client's tenant")
	assert.Equal(t, "successful", got.Status)
	got, err = jp.GetByJID(tenant.WithID(context.Background(), tenant.Default), "c1", "j1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// A multi-job started by a request is run and saved in the request's tenant, even after the request
// is done and its context cancelled.
func TestMultiJobRunSavedInRequestTenant(t *testing.T) {
	sequentialJobIDs(t)
	al, jp, cls, _ := typedRunFixture(t)
	done := make(chan bool)
	al.testDone = done

	abortOnErr := false
	ctx, cancel := context.WithCancel(tenant.WithID(context.Background(), "a"))
	_, err := al.StartMultiClientJob(ctx, &jobs.MultiJobRequest{
		ClientIDs:      []string{"c1", "c2", "c3"},
		Command:        "uname -a",
		TimeoutSec:     30,
		AbortOnError:   &abortOnErr,
		Username:       "tester",
		OrderedClients: cls,
	})
	require.NoError(t, err)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("multi-job did not finish")
	}

	got, err := jp.GetMultiJob(tenant.WithID(context.Background(), "a"), "jid-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Len(t, got.Jobs, 3, "every client's job is saved in the request's tenant")
	none, err := jp.GetMultiJob(tenant.WithID(context.Background(), tenant.Default), "jid-1")
	require.NoError(t, err)
	assert.Nil(t, none)
}

// A result still being processed when the connection ends (or the server shuts down) must be saved:
// the client's context is cancelled by then.
func TestCmdResultSavedAfterClientContextCancelled(t *testing.T) {
	jp := makeJobsProvider(t, DataSourceOptions, testLog)
	t.Cleanup(func() { jp.Close() })
	cl := &ClientListener{logger: testLog, server: &Server{
		config:          &chconfig.Config{Server: chconfig.ServerConfig{MaxRequestBytesClient: 1024 * 1024}},
		jobProvider:     jp,
		uiJobWebSockets: ws.NewWebSocketCache(),
	}}
	clientCtx, cancel := context.WithCancel(tenant.WithID(context.Background(), tenant.Default))
	cancel()

	_, err := cl.saveCmdResult(clientCtx, []byte(`{"jid":"j1","client_id":"c1","status":"successful","started_at":"2026-01-01T00:00:00Z"}`))
	require.NoError(t, err)
	got, err := jp.GetByJID(tenant.WithID(context.Background(), tenant.Default), "c1", "j1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "successful", got.Status)
}
