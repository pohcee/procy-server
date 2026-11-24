package notifications_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	oss "github.com/openrport/openrport/server/notifications"
	ossqlite "github.com/openrport/openrport/server/notifications/repository/sqlite"
	"github.com/openrport/openrport/server/repotest"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/refs"
	"github.com/openrport/openrport/share/tenant"

	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/notifications"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestContract(t *testing.T) {
	repotest.Notifications(t, func(t *testing.T) oss.Repository {
		d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
		return notifications.New(d.App)
	})
}

func notification() oss.NotificationDetails {
	return oss.NotificationDetails{
		RefID:  refs.GenerateIdentifiable("Problem"),
		Data:   oss.NotificationData{ContentType: oss.ContentTypeTextPlain, Target: "smtp", Recipients: []string{"x@example.com"}, Subject: "s", Content: "c"},
		State:  oss.ProcessingStateQueued,
		ID:     refs.GenerateIdentifiable(oss.NotificationType),
		Target: oss.TargetMail,
	}
}

func seed(t *testing.T, d *pgtest.DB) {
	t.Helper()
	s := notifications.New(d.App)
	for _, id := range []tenant.ID{repotest.TenantA, repotest.TenantB} {
		require.NoError(t, s.SetDone(repotest.Ctx(id), notification(), "out"))
	}
}

func TestRLSBackstop(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	seed(t, d)
	var tenants []string
	require.NoError(t, postgres.InTenant(repotest.Ctx(repotest.TenantA), d.App, func(tx *sqlx.Tx) error {
		return tx.Select(&tenants, "SELECT tenant_id FROM notifications_log")
	}))
	assert.Equal(t, []string{"a"}, tenants)
	var n int
	require.NoError(t, d.App.Get(&n, "SELECT count(*) FROM notifications_log"))
	assert.Zero(t, n)
}

func TestTenantDeleteCascades(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	seed(t, d)
	_, err := d.Owner.Exec("DELETE FROM tenants WHERE id = 'a'")
	require.NoError(t, err)
	var tenants []string
	require.NoError(t, d.Owner.Select(&tenants, "SELECT tenant_id FROM notifications_log"))
	assert.Equal(t, []string{"b"}, tenants)
}

// The server's cleaner runs on the store.
func TestCleanerRuns(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	seed(t, d)
	s := notifications.New(d.App)
	l := logger.NewLogger("cleaner", logger.LogOutput{File: os.Stdout}, logger.LogLevelError)
	c := ossqlite.StartCleaner(repotest.Ctx(repotest.TenantA), l, s, -time.Hour, time.Hour)
	defer c.Close()
	assert.Eventually(t, func() bool {
		n, err := s.Count(repotest.Ctx(repotest.TenantA), &query.ListOptions{})
		return err == nil && n == 0
	}, 5*time.Second, 20*time.Millisecond)
	n, err := s.Count(repotest.Ctx(repotest.TenantB), &query.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// The processor consumes the store's queues and logs the outcome in the notification's tenant.
type consumer struct{}

func (consumer) Process(context.Context, oss.NotificationDetails) (string, error) { return "sent", nil }
func (consumer) Target() oss.Target                                               { return oss.TargetMail }

func TestProcessorLogsInTheNotificationsTenant(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	s := notifications.New(d.App)
	l := logger.NewLogger("notifications", logger.LogOutput{File: os.Stdout}, logger.LogLevelError)
	p := oss.NewProcessor(l, s, consumer{})
	defer p.Close()

	id, err := oss.NewDispatcher(s).Dispatch(repotest.Ctx(repotest.TenantB), refs.GenerateIdentifiable("Problem"),
		oss.NotificationData{ContentType: oss.ContentTypeTextPlain, Target: "smtp", Subject: "s"})
	require.NoError(t, err)
	assert.Eventually(t, func() bool {
		got, found, err := s.Details(repotest.Ctx(repotest.TenantB), id.ID())
		return err == nil && found && got.State == oss.ProcessingStateDone && got.Out == "sent"
	}, 5*time.Second, 20*time.Millisecond)
	_, found, err := s.Details(repotest.Ctx(repotest.TenantA), id.ID())
	require.NoError(t, err)
	assert.False(t, found)
}

func TestTimestampFilterIsUTC(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	s := notifications.New(d.App)
	ctx := repotest.Ctx(repotest.TenantA)
	require.NoError(t, s.SetDone(ctx, notification(), ""))
	// a zone-less value is UTC whatever the server's time zone
	_, err := d.Owner.Exec("ALTER DATABASE " + d.Name + " SET TimeZone = 'Pacific/Kiritimati'")
	require.NoError(t, err)
	d.App.SetMaxIdleConns(0) // new connections pick the setting up
	var tz string
	require.NoError(t, d.App.Get(&tz, "SHOW TimeZone"))
	require.Equal(t, "Pacific/Kiritimati", tz)
	gt := func(at time.Time) int {
		n, err := s.Count(ctx, &query.ListOptions{Filters: []query.FilterOption{{Column: []string{"timestamp"}, Operator: query.FilterOperatorTypeGT, Values: []string{at.UTC().Format("2006-01-02 15:04:05")}}}})
		require.NoError(t, err)
		return n
	}
	assert.Equal(t, 1, gt(time.Now().Add(-2*time.Minute)))
	assert.Equal(t, 0, gt(time.Now().Add(2*time.Minute)))
}
