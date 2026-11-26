package chserver

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/api/jobs/schedule"
	"github.com/pohcee/procy-server/server/cgroups"
	"github.com/pohcee/procy-server/server/clients"
	"github.com/pohcee/procy-server/server/clients/clientdata"
	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/tenant"
)

// A cron-fired schedule resolves its client groups through the tenant-scoped provider, using the
// tenant the server's root context carries (standalone: the default tenant).
func TestScheduledRunResolvesGroupsWithDefaultTenant(t *testing.T) {
	root := tenant.WithID(context.Background(), tenant.Default)
	curUser := makeTestUser("test-user")

	conn := makeConnMock(t, 1, time.Date(2020, 10, 10, 10, 10, 1, 0, time.UTC))
	c1 := clients.New(t).ID("client-1").Connection(conn).Logger(testLog).Build()
	c1.SetAllowedUserGroups([]string{"group-1"})

	al := makeAPIListener(curUser, clients.NewClientRepository([]*clientdata.Client{c1}, &hour, testLog), 60, nil, testLog)
	done := make(chan bool, 10)
	al.testDone = done

	jp := makeJobsProvider(t, DataSourceOptions, testLog)
	defer jp.Close()
	gp := makeGroupsProvider(t, DataSourceOptions)
	defer gp.Close()
	al.jobProvider = jp
	al.clientGroupProvider = gp

	require.NoError(t, gp.Create(root, makeClientGroup("group-1", &cgroups.ClientParams{
		ClientID: &cgroups.ParamValues{"client-1"},
	})))

	m := schedule.NewManager(root, al.jobRunner, schedule.NewSQLiteProvider(jobsDBOf(t, jp)), testLog, 30)
	abort := false
	s, err := m.Create(root, &schedule.Schedule{
		Base: schedule.Base{Name: "every second", Schedule: "@every 1s", Type: schedule.TypeCommand},
		Details: schedule.Details{
			GroupIDs:     []string{"group-1"},
			Command:      "/bin/date",
			TimeoutSec:   30,
			AbortOnError: &abort,
		},
	}, "test-user")
	require.NoError(t, err)
	defer func() { _ = m.Delete(root, s.ID) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled run did not reach the group's client")
	}
}

// A cron-fired library schedule resolves its entry and client groups and saves its run, all in the
// tenant of the server's root context (standalone: the default tenant).
func TestScheduledLibraryRunUsesDefaultTenant(t *testing.T) {
	root := tenant.WithID(context.Background(), tenant.Default)
	e := newLibraryRunEnv(t)
	entry := e.createEntry(t, runEntryBody)
	done := make(chan bool, 10)
	e.al.testDone = done

	gp := e.al.clientGroupProvider
	require.NoError(t, gp.Create(root, makeClientGroup("group-1", &cgroups.ClientParams{
		ClientID: &cgroups.ParamValues{"c-linux"},
	})))

	m := schedule.NewManager(root, e.al.jobRunner, schedule.NewSQLiteProvider(jobsDBOf(t, e.jp)), testLog, 30)
	m.SetLibrary(e.al.libraryManager, e.jp)
	abort := false
	s, err := m.Create(root, &schedule.Schedule{
		Base:    schedule.Base{Name: "every second", Schedule: "@every 1s", Type: schedule.TypeLibrary},
		Details: schedule.Details{GroupIDs: []string{"group-1"}, LibraryEntryID: entry.ID, AbortOnError: &abort},
	}, "admin")
	require.NoError(t, err)
	defer func() { _ = m.Delete(root, s.ID) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduled library run did not reach the group's client")
	}

	runs, err := e.jp.GetMultiJobSummaries(root, &query.ListOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, runs)
	run, err := e.jp.GetMultiJob(root, runs[len(runs)-1].JID) // the oldest run is the one that finished
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, entry.ID, run.LibraryEntryID)
	require.Len(t, run.Jobs, 1, "the run's job is saved in the default tenant")
	assert.Equal(t, "c-linux", run.Jobs[0].ClientID)
}
