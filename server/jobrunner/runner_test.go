package jobrunner

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	jobsmigration "github.com/openrport/openrport/db/migration/jobs"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/server/clients/clienttunnel"
	"github.com/openrport/openrport/server/dispatch"
	"github.com/openrport/openrport/share/comm"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/tenant"
)

var testLog = logger.NewLogger("jobrunner-test", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug)

// orderDispatcher records the clients it runs jobs on; c-down is not connected.
type orderDispatcher struct {
	dispatch.Dispatcher // only RunJob is used
	mu                  sync.Mutex
	ids                 []string
}

func (d *orderDispatcher) RunJob(_ context.Context, clientID string, _ *models.Job) (*comm.RunCmdResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ids = append(d.ids, clientID)
	if clientID == "c-down" {
		return nil, dispatch.ErrClientNotConnected
	}
	return &comm.RunCmdResponse{Pid: 1, StartedAt: time.Now()}, nil
}

func (d *orderDispatcher) StartTunnels(context.Context, string, []*models.Remote) ([]*clienttunnel.Tunnel, error) {
	return nil, nil
}

type noTargets struct{}

func (noTargets) Ordered(context.Context, []string, []string) ([]*clientdata.Client, int, error) {
	return nil, 0, nil
}
func (noTargets) ByTag(context.Context, *models.JobClientTags) ([]*clientdata.Client, error) {
	return nil, nil
}

func TestNewRequiresDependencies(t *testing.T) {
	_, err := New(Options{})
	assert.ErrorContains(t, err, "Jobs is required")
}

// A runner built only from its options (no API, no client listener) runs a sequential
// multi-client job: each client in order, a client that is not connected does not abort the
// run, and every job is saved in the tenant of ctx.
func TestRunnerWithoutAPI(t *testing.T) {
	db, err := sqlite.New(":memory:", jobsmigration.FS, sqlite.DataSourceOptions{})
	require.NoError(t, err)
	jp := jobs.NewSqliteProvider(db, testLog)
	t.Cleanup(func() { _ = jp.Close() })

	d := &orderDispatcher{}
	done := make(chan struct{}, 1)
	n := 0
	r, err := New(Options{
		Jobs:       jp,
		Dispatcher: d,
		Targets:    noTargets{},
		Config:     &chconfig.Config{Server: chconfig.ServerConfig{RunRemoteCmdTimeoutSec: 60}},
		Done:       NewDoneChannels(),
		Logger:     testLog,
		NewJobID: func() (string, error) {
			n++
			return "jid-" + strconv.Itoa(n), nil
		},
		OnRunDone:        func() { done <- struct{}{} },
		NoSequentialWait: true,
	})
	require.NoError(t, err)

	cls := []*clientdata.Client{{ID: "c1"}, {ID: "c-down"}, {ID: "c2"}}
	abort := true
	ctx := tenant.WithID(context.Background(), "a")
	mj, err := r.StartMultiClientJob(ctx, &jobs.MultiJobRequest{
		ClientIDs:      []string{"c1", "c-down", "c2"},
		Command:        "date",
		AbortOnError:   &abort,
		Username:       "tester",
		OrderedClients: cls,
	})
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not finish")
	}

	assert.Equal(t, []string{"c1", "c-down", "c2"}, d.ids)
	got, err := jp.GetMultiJob(ctx, mj.JID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Len(t, got.Jobs, 3, "the failed job of a multi-job is saved too")
	assert.Equal(t, 60, got.TimeoutSec, "the configured default timeout")
	other, err := jp.GetMultiJob(tenant.WithID(context.Background(), tenant.Default), mj.JID)
	require.NoError(t, err)
	assert.Nil(t, other)
}
