// Package monitoring is the Postgres monitoring.DBProvider: the measurements clients send, per tenant, and the
// downsampled graphs of them.
package monitoring

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/server/monitoring"
	"github.com/pohcee/procy-server/share/models"
	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/tenant"

	"github.com/pohcee/procy-server/server/storage/postgres"
)

// MaxDeletedEntries is how many distinct measurement times one DeleteMeasurementsBefore removes at most, as in
// SQLite.
const MaxDeletedEntries = monitoring.MaxDeletedEntries

// Store keeps measurements in the table measurements.
type Store struct {
	db        *sqlx.DB
	converter *query.SQLConverter
}

var _ monitoring.DBProvider = (*Store)(nil)

// New returns the monitoring store on db (shared; Close leaves it open).
func New(db *sqlx.DB) *Store {
	return &Store{db: db, converter: query.NewSQLConverter("postgres")}
}

// inTenant runs fn in a transaction of ctx's tenant, with times read and written in UTC (the API passes filter
// times without a zone).
func (s *Store) inTenant(ctx context.Context, fn func(tx *sqlx.Tx, tid tenant.ID) error) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	return postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, "SET LOCAL TIME ZONE 'UTC'"); err != nil {
			return err
		}
		return fn(tx, tid)
	})
}

func options(o *query.ListOptions) query.ListOptions {
	if o == nil {
		return query.ListOptions{}
	}
	return *o
}

// list runs "SELECT * FROM measurements AS <alias>" for the client with the list options (whose fields name the
// columns) into dest.
func (s *Store) list(ctx context.Context, alias, clientID string, o *query.ListOptions, dest interface{}) error {
	return s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		lo := options(o)
		q, params := s.converter.AppendOptionsToQuery(&lo, "SELECT * FROM measurements AS "+alias+" WHERE tenant_id = ? AND client_id = ? ", []interface{}{tid, clientID})
		return tx.SelectContext(ctx, dest, tx.Rebind(q), params...)
	})
}

func (s *Store) ListMountpointsByClientID(ctx context.Context, clientID string, o *query.ListOptions) ([]*monitoring.ClientMountpointsPayload, error) {
	val := []*monitoring.ClientMountpointsPayload{}
	err := s.list(ctx, "mountpoints", clientID, o, &val)
	for _, v := range val {
		v.Timestamp = v.Timestamp.UTC()
	}
	return val, err
}

func (s *Store) ListProcessesByClientID(ctx context.Context, clientID string, o *query.ListOptions) ([]*monitoring.ClientProcessesPayload, error) {
	val := []*monitoring.ClientProcessesPayload{}
	err := s.list(ctx, "processes", clientID, o, &val)
	for _, v := range val {
		v.Timestamp = v.Timestamp.UTC()
	}
	return val, err
}

func (s *Store) ListMetricsByClientID(ctx context.Context, clientID string, o *query.ListOptions) ([]*monitoring.ClientMetricsPayload, error) {
	val := []*monitoring.ClientMetricsPayload{}
	err := s.list(ctx, "metrics", clientID, o, &val)
	for _, v := range val {
		v.Timestamp = v.Timestamp.UTC()
	}
	return val, err
}

// CountByClientID counts the client's measurements matching the filters of options.
func (s *Store) CountByClientID(ctx context.Context, clientID string, o *query.ListOptions) (int, error) {
	var n int
	err := s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		lo := options(o)
		lo.Pagination = nil
		lo.Sorts = nil
		lo.Fields = nil
		q, params := s.converter.AppendOptionsToQuery(&lo, "SELECT count(*) FROM measurements WHERE tenant_id = ? AND client_id = ? ", []interface{}{tid, clientID})
		return tx.GetContext(ctx, &n, tx.Rebind(q), params...)
	})
	return n, err
}

// aggregates selects the rounded average, minimum and maximum of field as alias_avg, alias_min and alias_max.
func aggregates(field, alias string) string {
	return fmt.Sprintf("round(avg(%[1]s)::numeric, 2)::float8 AS %[2]s_avg, min(%[1]s)::float8 AS %[2]s_min, max(%[1]s)::float8 AS %[2]s_max", field, alias)
}

// graph runs the downsampling query: the client's measurements matching the filters of lo are grouped into
// buckets of hours*29 seconds (SQLite: GROUP BY round(strftime('%s', timestamp) / divisor)), each bucket timed
// by its first measurement.
func (s *Store) graph(ctx context.Context, clientID string, hours float64, lo *query.ListOptions, selects string, dest interface{}) error {
	return s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		o := options(lo)
		q, params := s.converter.AddWhere(o.Filters, "SELECT min(timestamp) AS timestamp, "+selects+" FROM measurements WHERE tenant_id = ? AND client_id = ?", []interface{}{tid, clientID})
		/*This is the part of "downsampling graph data" (group together graph points, so that you don't get too much points in one request).
		The value of "29" comes from Thorsten. He did some research and found out that "29" would be the best fit.
		*/
		q += " GROUP BY round(extract(epoch FROM timestamp) / ?::float8)"
		params = append(params, (math.Round(hours*100)/100)*29)
		q = s.converter.AddOrderBy(o.Sorts, q)
		return tx.SelectContext(ctx, dest, tx.Rebind(q), params...)
	})
}

func (s *Store) ListGraphMetricsByClientID(ctx context.Context, clientID string, hours float64, lo *query.ListOptions) ([]*monitoring.ClientGraphMetricsPayload, error) {
	val := []*monitoring.ClientGraphMetricsPayload{}
	selects := strings.Join([]string{
		aggregates("cpu_usage_percent", "cpu_usage_percent"),
		aggregates("memory_usage_percent", "memory_usage_percent"),
		aggregates("io_usage_percent", "io_usage_percent"),
	}, ", ")
	err := s.graph(ctx, clientID, hours, lo, selects, &val)
	for _, v := range val {
		v.Timestamp = v.Timestamp.UTC()
	}
	return val, err
}

func (s *Store) ListGraphByClientID(ctx context.Context, clientID string, hours float64, lo *query.ListOptions, graph string) ([]*monitoring.ClientGraphMetricsGraphPayload, error) {
	if _, err := postgres.TenantOf(ctx); err != nil {
		return nil, err
	}
	field, okField := monitoring.ClientGraphNameToField[graph]
	alias, okAlias := monitoring.ClientGraphNameToAlias[graph]
	if !okField || !okAlias {
		return nil, fmt.Errorf("unknown graph: %s", graph)
	}
	selects := aggregates(field, alias)
	if strings.HasPrefix(graph, "net_") {
		selects += ", " + aggregates(strings.ReplaceAll(field, "_in", "_out"), strings.ReplaceAll(alias, "_in", "_out"))
	}
	val := []*monitoring.ClientGraphMetricsGraphPayload{}
	err := s.graph(ctx, clientID, hours, lo, selects, &val)
	for _, v := range val {
		v.Timestamp = v.Timestamp.UTC()
	}
	return val, err
}

// CreateMeasurement stores a measurement; a client has one measurement per time.
func (s *Store) CreateMeasurement(ctx context.Context, m *models.Measurement) error {
	return s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		var lanIn, lanOut, wanIn, wanOut *int
		if m.NetLan != nil {
			lanIn, lanOut = &m.NetLan.In, &m.NetLan.Out
		}
		if m.NetWan != nil {
			wanIn, wanOut = &m.NetWan.In, &m.NetWan.Out
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO measurements (tenant_id, client_id, timestamp, cpu_usage_percent, memory_usage_percent, io_usage_percent, processes, mountpoints, net_lan_in, net_lan_out, net_wan_in, net_wan_out)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			tid, m.ClientID, m.Timestamp, m.CPUUsagePercent, m.MemoryUsagePercent, m.IoUsagePercent, m.Processes, m.Mountpoints, lanIn, lanOut, wanIn, wanOut)
		return err
	})
}

// DeleteMeasurementsBefore deletes the tenant's measurements older than compare, of at most MaxDeletedEntries
// distinct times; call it until it returns 0 to delete them all.
func (s *Store) DeleteMeasurementsBefore(ctx context.Context, compare time.Time) (int64, error) {
	var n int64
	err := s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM measurements WHERE tenant_id = $1 AND timestamp IN
			 (SELECT DISTINCT timestamp FROM measurements WHERE tenant_id = $1 AND timestamp < $2 ORDER BY timestamp LIMIT $3)`,
			tid, compare, MaxDeletedEntries)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// Close does nothing: the database is shared and closed by its owner.
func (s *Store) Close() error { return nil }
