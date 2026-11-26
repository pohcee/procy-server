// Package auditlog is the Postgres auditlog.Provider: one audit log table for all tenants, without rotation.
package auditlog

import (
	"context"

	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/server/auditlog"
	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/tenant"

	"github.com/pohcee/procy-server/server/storage/postgres"
)

// Store keeps the audit log in the table auditlog.
type Store struct {
	db        *sqlx.DB
	converter *query.SQLConverter
}

var _ auditlog.Provider = (*Store)(nil)

// New returns the audit log store on db (shared; Close leaves it open).
func New(db *sqlx.DB) *Store {
	return &Store{db: db, converter: query.NewSQLConverter("postgres")}
}

// inTenant runs fn in a transaction of ctx's tenant, with times read and written in UTC (filter values without a
// zone are UTC).
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

// Save stores the entry for the tenant carried by ctx and sets its TenantID to it.
func (s *Store) Save(ctx context.Context, e *auditlog.Entry) error {
	return s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		e.TenantID = tid
		_, err := tx.ExecContext(ctx,
			`INSERT INTO auditlog (tenant_id, timestamp, username, remote_ip, application, action, affected_id, client_id, client_hostname, request, response)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			tid, e.Timestamp, e.Username, e.RemoteIP, e.Application, e.Action, e.ID, e.ClientID, e.ClientHostName, e.Request, e.Response)
		return err
	})
}

// listColumns reads the nullable text columns as ” under their own names, so sorts by them still work.
const listColumns = `timestamp, coalesce(username, '') AS username, coalesce(remote_ip, '') AS remote_ip, application, action,
	coalesce(affected_id, '') AS affected_id, coalesce(client_id, '') AS client_id, coalesce(client_hostname, '') AS client_hostname,
	coalesce(request, '') AS request, coalesce(response, '') AS response, tenant_id`

// List returns the entries matching options, in insertion order unless options sort otherwise.
func (s *Store) List(ctx context.Context, options *query.ListOptions) ([]*auditlog.Entry, error) {
	values := []*auditlog.Entry{}
	err := s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		o := query.ListOptions{}
		if options != nil {
			o = *options
		}
		// equal sort values keep the insertion order, as SQLite's rowid does
		asc := len(o.Sorts) == 0 || o.Sorts[0].IsASC
		o.Sorts = append(append([]query.SortOption{}, o.Sorts...), query.SortOption{Column: "id", IsASC: asc})
		q, params := s.converter.AppendOptionsToQuery(&o, "SELECT "+listColumns+" FROM auditlog WHERE tenant_id = ?", []interface{}{tid})
		if err := tx.SelectContext(ctx, &values, tx.Rebind(q), params...); err != nil {
			return err
		}
		for _, v := range values {
			v.Timestamp = v.Timestamp.UTC()
		}
		return nil
	})
	return values, err
}

// Count counts the entries matching options (pagination ignored).
func (s *Store) Count(ctx context.Context, options *query.ListOptions) (int, error) {
	var n int
	err := s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		o := query.ListOptions{}
		if options != nil {
			o = *options
		}
		o.Pagination = nil
		o.Sorts = nil
		q, params := s.converter.AppendOptionsToQuery(&o, "SELECT count(*) FROM auditlog WHERE tenant_id = ?", []interface{}{tid})
		return tx.GetContext(ctx, &n, tx.Rebind(q), params...)
	})
	return n, err
}

// Close does nothing: the database is shared and closed by its owner.
func (s *Store) Close() error { return nil }
