package chserver

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api/jobs/schedule"
	"github.com/openrport/openrport/server/cgroups"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/share/tenant"
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

	m := schedule.NewManager(root, al, jp.GetDB(), testLog, 30)
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
