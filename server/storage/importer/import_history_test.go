package importer

import (
	"sort"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	auditlogmigration "github.com/openrport/openrport/db/migration/auditlog"
	jobsmigration "github.com/openrport/openrport/db/migration/jobs"
	monitoringmigration "github.com/openrport/openrport/db/migration/monitoring"
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/auditlog"
	"github.com/openrport/openrport/server/monitoring"
	"github.com/openrport/openrport/server/notifications"
	notifsqlite "github.com/openrport/openrport/server/notifications/repository/sqlite"
	pgauditlog "github.com/openrport/openrport/server/storage/postgres/auditlog"
	pgjobs "github.com/openrport/openrport/server/storage/postgres/jobs"
	pgmonitoring "github.com/openrport/openrport/server/storage/postgres/monitoring"
	pgnotifications "github.com/openrport/openrport/server/storage/postgres/notifications"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/refs"
)

func page() *query.ListOptions { return &query.ListOptions{Pagination: query.NewPagination(100, 0)} }

func metricsPage() *query.ListOptions {
	return &query.ListOptions{
		Sorts:      query.ParseSortOptions(map[string][]string{"sort": {"-timestamp"}}),
		Fields:     query.ParseFieldsOptions(monitoring.ClientMetricsFieldsDefault),
		Pagination: query.NewPagination(100, 0),
	}
}

// history is the history a standalone data dir holds, as its SQLite stores return it.
type history struct {
	multiJob      *models.MultiJob
	notifications []notifications.NotificationSummary
	notifDetails  notifications.NotificationDetails
	audit         []*auditlog.Entry
	metrics       []*monitoring.ClientMetricsPayload
}

func writeHistory(t *testing.T, d *dataDir) *history {
	t.Helper()
	h := &history{}
	start := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)
	d.with("jobs.db", jobsmigration.FS, func(db *sqlx.DB) {
		p := jobs.NewSqliteProvider(db, testLog)
		sched := "s1"
		mj := &models.MultiJob{
			MultiJobSummary: models.MultiJobSummary{JID: "mj1", StartedAt: start, CreatedBy: "admin", ScheduleID: &sched},
			ClientIDs:       []string{"c1"}, Command: "echo a\x00b", Interpreter: "/bin/sh", TimeoutSec: 60,
			Error: "could not start: entry \x00 gone",
		}
		require.NoError(t, p.SaveMultiJob(ctxDefault, mj))
		finished := start.Add(time.Minute)
		mid := "mj1"
		require.NoError(t, p.CreateJob(ctxDefault, &models.Job{JID: "j1", Status: models.JobStatusSuccessful, ClientID: "c1", ClientName: "one",
			Command: "uptime", StartedAt: start, FinishedAt: &finished, CreatedBy: "admin", MultiJobID: &mid, Result: &models.JobResult{StdOut: "up"}}))
		gone := "gone"
		require.NoError(t, p.CreateJob(ctxDefault, &models.Job{JID: "j2", Status: models.JobStatusRunning, ClientID: "c1", StartedAt: start, CreatedBy: "admin", MultiJobID: &gone}))
		var err error
		h.multiJob, err = p.GetMultiJob(ctxDefault, "mj1")
		require.NoError(t, err)
	})
	d.with("notifications.db", notifsqlite.Migrations, func(db *sqlx.DB) {
		r := notifsqlite.NewRepository(db, testLog)
		n := notifications.NotificationDetails{
			RefID: refs.GenerateIdentifiable("Problem"),
			Data: notifications.NotificationData{ContentType: notifications.ContentTypeTextPlain, Target: "smtp",
				Recipients: []string{"ops@example.com"}, Subject: "disk full", Content: "the disk is full"},
			State: notifications.ProcessingStateQueued, ID: refs.GenerateIdentifiable(notifications.NotificationType), Target: notifications.TargetMail,
		}
		require.NoError(t, r.SetError(ctxDefault, n, "out", "smtp refused"))
		require.NoError(t, r.SetDone(ctxDefault, n, "sent"))
		var err error
		h.notifications, err = r.List(ctxDefault, page())
		require.NoError(t, err)
		var found bool
		h.notifDetails, found, err = r.Details(ctxDefault, n.ID.ID())
		require.NoError(t, err)
		require.True(t, found)
		require.NoError(t, r.Close())
	})
	// the current audit log and a rotated one
	for i, name := range []string{"auditlog.2026-03-01.db", "auditlog.db"} {
		d.with(name, auditlogmigration.FS, func(db *sqlx.DB) {
			p := auditlog.NewSQLiteProvider(db)
			e := &auditlog.Entry{Timestamp: start.Add(time.Duration(i) * time.Hour), Username: "admin", RemoteIP: "192.0.2.9", Application: "client", Action: "delete",
				ID: "c1", ClientID: "c1", Request: `{"x":1}`}
			require.NoError(t, p.Save(ctxDefault, e))
			list, err := p.List(ctxDefault, page())
			require.NoError(t, err)
			h.audit = append(h.audit, list...)
		})
	}
	d.with("monitoring.db", monitoringmigration.FS, func(db *sqlx.DB) {
		p := monitoring.NewSqliteProvider(db, testLog)
		for i := 0; i < 3; i++ {
			require.NoError(t, p.CreateMeasurement(ctxDefault, &models.Measurement{ClientID: "c1", Timestamp: start.Add(time.Duration(i) * time.Minute),
				CPUUsagePercent: float64(10 + i), MemoryUsagePercent: 20.5, IoUsagePercent: 1, Processes: "[]", Mountpoints: "{}",
				NetLan: &models.NetBytes{In: 100, Out: 200}}))
		}
		var err error
		h.metrics, err = p.ListMetricsByClientID(ctxDefault, "c1", metricsPage())
		require.NoError(t, err)
	})
	return h
}

func TestImportHistoryOnlyWithFlag(t *testing.T) {
	pg := pgtest.New(t)
	d := newDataDir(t)
	h := writeHistory(t, d)

	rep := run(t, pg, Options{From: d.dir})
	want(t, rep, entMultiJobs, 1, 0, 0, 1)
	want(t, rep, entJobs, 2, 0, 0, 2)
	want(t, rep, entNotifications, 2, 0, 0, 2)
	want(t, rep, entAuditLog, 2, 0, 0, 2)
	want(t, rep, entMonitoring, 3, 0, 0, 3)
	assert.True(t, hasWarning(rep, "jobs: 2 rows not imported (use --with-history)"), rep.Warnings)
	var n int
	require.NoError(t, pg.Owner.Get(&n, "SELECT (SELECT count(*) FROM jobs) + (SELECT count(*) FROM multi_jobs) + (SELECT count(*) FROM auditlog) + (SELECT count(*) FROM measurements) + (SELECT count(*) FROM notifications_log)"))
	assert.Zero(t, n)

	rep = run(t, pg, Options{From: d.dir, WithHistory: true})
	want(t, rep, entMultiJobs, 1, 1, 0, 0)
	want(t, rep, entJobs, 2, 1, 0, 1)
	want(t, rep, entNotifications, 2, 2, 0, 0)
	want(t, rep, entAuditLog, 2, 2, 0, 0)
	want(t, rep, entMonitoring, 3, 3, 0, 0)
	assert.True(t, hasWarning(rep, "jobs: 1 jobs of multi-client jobs that don't exist"), rep.Warnings)

	// jobs, with run_error filled from details (NULs dropped)
	gotMJ, err := pgjobs.New(pg.App).GetMultiJob(ctxDefault, "mj1")
	require.NoError(t, err)
	sameJSON(t, h.multiJob, gotMJ)
	var runError string
	require.NoError(t, pg.Owner.Get(&runError, "SELECT run_error FROM multi_jobs WHERE jid = 'mj1'"))
	assert.Equal(t, "could not start: entry  gone", runError)

	ns := pgnotifications.New(pg.App)
	gotN, err := ns.List(ctxDefault, page())
	require.NoError(t, err)
	sameJSON(t, h.notifications, gotN)
	gotD, found, err := ns.Details(ctxDefault, h.notifDetails.ID.ID())
	require.NoError(t, err)
	require.True(t, found)
	sameJSON(t, h.notifDetails, gotD)

	gotA, err := pgauditlog.New(pg.App).List(ctxDefault, page())
	require.NoError(t, err)
	sort.Slice(h.audit, func(i, j int) bool { return h.audit[i].Timestamp.Before(h.audit[j].Timestamp) })
	sameJSON(t, h.audit, gotA)

	gotM, err := pgmonitoring.New(pg.App).ListMetricsByClientID(ctxDefault, "c1", metricsPage())
	require.NoError(t, err)
	sameJSON(t, h.metrics, gotM)

	// a new notification gets an id above the imported ones
	var maxID, next int64
	require.NoError(t, pg.Owner.Get(&maxID, "SELECT max(id) FROM notifications_log"))
	require.NoError(t, pg.Owner.Get(&next, "SELECT nextval(pg_get_serial_sequence('notifications_log', 'id'))"))
	assert.Greater(t, next, maxID)
}
