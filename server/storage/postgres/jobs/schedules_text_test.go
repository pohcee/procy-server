package jobs_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/api/jobs/schedule"
	"github.com/pohcee/procy-server/server/repotest"
	"github.com/pohcee/procy-server/server/test/jb"
	"github.com/pohcee/procy-server/share/query"

	"github.com/pohcee/procy-server/server/storage/postgres/jobs"
	"github.com/pohcee/procy-server/server/storage/postgres/pgtest"
)

// Go writes a NUL in a command as \u0000, which jsonb refuses: a run with one must not break the schedule list.
func TestScheduleListWithNULInRunDetails(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	js, ss := jobs.New(d.App), jobs.NewScheduleStore(d.App)
	ctx := repotest.Ctx(repotest.TenantA)
	require.NoError(t, ss.Insert(ctx, &schedule.Schedule{Base: schedule.Base{ID: "s1", Name: "s", Type: schedule.TypeCommand}}))
	mj := jb.NewMulti(t).JID("mj1").ScheduleID("s1").Build()
	mj.Command = "echo a\x00b"
	mj.Error = "failed"
	require.NoError(t, js.SaveMultiJob(ctx, mj))

	list, err := ss.List(ctx, &query.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.NotNil(t, list[0].LastExecution)
	require.NotNil(t, list[0].LastExecution.Error)
	assert.Equal(t, "failed", *list[0].LastExecution.Error)
}
