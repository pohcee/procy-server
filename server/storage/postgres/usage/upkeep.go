package usage

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/server/storage/postgres"
	"github.com/pohcee/procy-server/share/tenant"
)

// Default retention of the usage tables (billing and metering design).
const (
	DefaultHourlyRetention = 90 * 24 * time.Hour
	DefaultDailyRetention  = 25 * 31 * 24 * time.Hour
	// MinHourlyRetention: hours are rolled up into days for 9 days after they end, so they must be kept longer.
	MinHourlyRetention = 10 * 24 * time.Hour
)

// Retention is how long the usage tables are kept: Hourly for usage_hourly and usage_client_daily, Daily for
// usage_daily and usage_device_days. Zero fields take the defaults.
type Retention struct {
	Hourly, Daily time.Duration
	// Monitoring records the monitoring rows gauge (nodes with [metering] on).
	Monitoring bool
}

func (r Retention) withDefaults() Retention {
	if r.Hourly <= 0 {
		r.Hourly = DefaultHourlyRetention
	}
	if r.Hourly < MinHourlyRetention {
		r.Hourly = MinHourlyRetention
	}
	if r.Daily <= 0 {
		r.Daily = DefaultDailyRetention
	}
	if r.Daily < r.Hourly {
		r.Daily = r.Hourly
	}
	return r
}

// Upkeep does the periodic work on the usage of every tenant (the cluster upkeep calls it under its lease, on every
// Postgres node, so a node without [metering] holding the lease still rolls up and prunes): it records the
// monitoring rows gauge once a day (r.Monitoring), rolls the hours of the days that ended more than two days ago up into
// usage_daily (recomputing the last week, which late data may still change, and any day not rolled up yet), and
// prunes what is older than r.
func Upkeep(ctx context.Context, db *sqlx.DB, r Retention) error {
	r = r.withDefaults()
	var ids []string
	if err := db.SelectContext(ctx, &ids, "SELECT id FROM tenants ORDER BY id"); err != nil {
		return fmt.Errorf("usage upkeep: %w", err)
	}
	var firstErr error
	for _, id := range ids {
		if err := upkeepTenant(ctx, db, tenant.ID(id), r); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("usage upkeep of tenant %s: %w", id, err)
		}
	}
	return firstErr
}

func upkeepTenant(ctx context.Context, db *sqlx.DB, tid tenant.ID, r Retention) error {
	return postgres.InTenantID(ctx, db, tid, func(tx *sqlx.Tx) error {
		stmts := []struct {
			q    string
			args []any
		}{
			// the monitoring rows stored, once a day (at the day's first hour)
			{`INSERT INTO usage_hourly (tenant_id, hour, metric, value)
				SELECT $1, date_trunc('day', now(), 'UTC'), 'monitoring_rows', n
				FROM (SELECT count(*) AS n FROM measurements WHERE tenant_id = $1) c
				WHERE n > 0 AND NOT EXISTS (SELECT 1 FROM usage_hourly WHERE tenant_id = $1
					AND hour = date_trunc('day', now(), 'UTC') AND metric = 'monitoring_rows')`, nil},
			{`INSERT INTO usage_daily (tenant_id, day, metric, value)
				SELECT h.tenant_id, (h.hour AT TIME ZONE 'UTC')::date, h.metric,
					(CASE WHEN rport_usage_is_gauge(h.metric) THEN max(h.value) ELSE sum(h.value) END)::bigint
				FROM usage_hourly h
				WHERE h.tenant_id = $1 AND h.hour < (((now() AT TIME ZONE 'UTC')::date - 2)::timestamp AT TIME ZONE 'UTC')
					AND (h.hour >= (((now() AT TIME ZONE 'UTC')::date - 9)::timestamp AT TIME ZONE 'UTC')
						OR NOT EXISTS (SELECT 1 FROM usage_daily d WHERE d.tenant_id = $1
							AND d.day = (h.hour AT TIME ZONE 'UTC')::date))
				GROUP BY 1, 2, 3
				ON CONFLICT (tenant_id, day, metric) DO UPDATE SET value = EXCLUDED.value`, nil},
			{"DELETE FROM usage_hourly WHERE tenant_id = $1 AND hour < now() - make_interval(secs => $2)", []any{r.Hourly.Seconds()}},
			{"DELETE FROM usage_client_daily WHERE tenant_id = $1 AND day < ((now() - make_interval(secs => $2)) AT TIME ZONE 'UTC')::date", []any{r.Hourly.Seconds()}},
			{"DELETE FROM usage_daily WHERE tenant_id = $1 AND day < ((now() - make_interval(secs => $2)) AT TIME ZONE 'UTC')::date", []any{r.Daily.Seconds()}},
			{"DELETE FROM usage_device_days WHERE tenant_id = $1 AND day < ((now() - make_interval(secs => $2)) AT TIME ZONE 'UTC')::date", []any{r.Daily.Seconds()}},
		}
		if !r.Monitoring {
			stmts = stmts[1:]
		}
		for _, s := range stmts {
			if _, err := tx.ExecContext(ctx, s.q, append([]any{string(tid)}, s.args...)...); err != nil {
				return err
			}
		}
		return nil
	})
}
