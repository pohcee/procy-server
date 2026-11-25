// Package usage is the Postgres storage of usage metering (migration 1300): Writer adds a node's flushed usage
// through ingest_usage (nodes without a device role; device processes ship the same items from their outbox),
// Store reads it for the usage API, and Upkeep rolls hours up into days, records the monitoring gauge and prunes.
package usage

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/metering"
	"github.com/openrport/openrport/server/storage/outbox"
	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/device"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

// batchSize is the most items per ingest_usage call (the function takes up to 1000).
const batchSize = 200

// Writer is the metering.Writer of a node connected as the application role: it calls ingest_usage directly, the
// function a device process's outbox ships the same items to.
type Writer struct {
	DB  *sqlx.DB
	Log *logger.Logger
}

var _ metering.Writer = Writer{}

// WriteUsage ships recs. Rejected records are logged and dropped (sending them again can't succeed); an error
// means the database didn't answer, and the Meter retries the same records.
func (w Writer) WriteUsage(ctx context.Context, recs []metering.Record) error {
	items := make([]outbox.Item, 0, len(recs))
	for _, r := range recs {
		p, err := r.Payload()
		if err != nil {
			return err
		}
		items = append(items, outbox.Item{Kind: outbox.KindUsage, Tenant: r.Tenant, Key: r.Key, Payload: p})
	}
	sink := device.Sink{DB: w.DB}
	for len(items) > 0 {
		n := min(len(items), batchSize)
		statuses, err := sink.Ship(ctx, outbox.KindUsage, items[:n])
		if err != nil {
			return err
		}
		for _, st := range statuses {
			if len(st.Outcome) >= 8 && st.Outcome[:8] == "rejected" && w.Log != nil {
				w.Log.Errorf("metering: usage %q %s", st.Key, st.Outcome)
			}
		}
		items = items[n:]
	}
	return nil
}

// Store reads the usage for the API, connected as the application role.
type Store struct {
	DB *sqlx.DB
}

var _ metering.Reader = Store{}

// Usage reads tid's usage in q: from usage_hourly for hourly periods, from usage_periods (daily rollups and the
// recent hours) for days and months.
func (s Store) Usage(ctx context.Context, tid tenant.ID, q metering.Query) (metering.Data, error) {
	var d metering.Data
	src := "usage_periods"
	if q.Granularity == metering.Hour {
		src = "usage_hourly"
	}
	err := postgres.InTenantID(ctx, s.DB, tid, func(tx *sqlx.Tx) error {
		// src and the granularity are constants
		if err := tx.SelectContext(ctx, &d.Periods, `SELECT date_trunc($2, hour, 'UTC') AS period, metric,
				(CASE WHEN rport_usage_is_gauge(metric) THEN max(value) ELSE sum(value) END)::bigint AS value
			FROM `+src+` WHERE tenant_id = $1 AND hour >= $3 AND hour < $4
			GROUP BY 1, 2 ORDER BY 1, 2`, string(tid), string(q.Granularity), q.From, q.To); err != nil {
			return err
		}
		if err := tx.SelectContext(ctx, &d.Days, `SELECT (day::timestamp AT TIME ZONE 'UTC') AS day, count(*) AS devices
			FROM usage_device_days WHERE tenant_id = $1 AND day >= ($2 AT TIME ZONE 'UTC')::date AND day < ($3 AT TIME ZONE 'UTC')::date
			GROUP BY day ORDER BY day`, string(tid), q.From, q.To.Add(-time.Nanosecond).Add(24*time.Hour).Truncate(24*time.Hour)); err != nil {
			return err
		}
		return tx.SelectContext(ctx, &d.Top, `WITH totals AS (
				SELECT client_id, metric, sum(value)::bigint AS value FROM usage_client_daily
				WHERE tenant_id = $1 AND day >= ($2 AT TIME ZONE 'UTC')::date AND day <= ($3 AT TIME ZONE 'UTC')::date
				GROUP BY client_id, metric),
			top AS (SELECT client_id FROM totals GROUP BY client_id ORDER BY sum(value) DESC, client_id LIMIT $4)
			SELECT t.client_id, t.metric, t.value FROM totals t JOIN top USING (client_id)`,
			string(tid), q.From, q.To.Add(-time.Nanosecond), metering.TopClients)
	})
	if err != nil {
		return d, fmt.Errorf("usage: %w", err)
	}
	return d, nil
}

// Summary reads every tenant's usage between from and to (rport_usage_summary, rport_usage_device_summary).
func (s Store) Summary(ctx context.Context, from, to time.Time) ([]metering.TenantRow, []metering.TenantDays, error) {
	var rows []metering.TenantRow
	var days []metering.TenantDays
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("usage: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.SelectContext(ctx, &rows, "SELECT tenant_id, metric, value FROM rport_usage_summary($1, $2)", from, to); err != nil {
		return nil, nil, fmt.Errorf("usage: summary: %w", err)
	}
	if err := tx.SelectContext(ctx, &days, `SELECT tenant_id, device_days, peak_day
		FROM rport_usage_device_summary($1::date, $2::date)`, dayOf(from), dayOf(to.Add(-time.Nanosecond).AddDate(0, 0, 1))); err != nil {
		return nil, nil, fmt.Errorf("usage: summary: %w", err)
	}
	return rows, days, nil
}

// dayOf is the UTC day of t, as a date string Postgres reads as a date.
func dayOf(t time.Time) string { return t.UTC().Format("2006-01-02") }
