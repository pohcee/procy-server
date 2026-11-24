package auditlog

import (
	"context"
	"fmt"
	"path"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/db/migration/auditlog"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
)

// SQLiteProvider stores audit log entries. Save, List and Count are scoped to the tenant carried by their context
// and fail with tenant.ErrMissing when there is none.
type SQLiteProvider struct {
	db        *sqlx.DB
	converter *query.SQLConverter
}

var _ Provider = (*SQLiteProvider)(nil)

// NewSQLiteProvider stores the entries in db, an opened and migrated audit log database.
func NewSQLiteProvider(db *sqlx.DB) *SQLiteProvider {
	return &SQLiteProvider{
		db:        db,
		converter: query.NewSQLConverter(db.DriverName()),
	}
}

// openSQLiteProvider opens auditlog.db in dataDir and always migrates it: rotation creates new files at runtime.
func openSQLiteProvider(dataDir string, dataSourceOptions sqlite.DataSourceOptions) (*SQLiteProvider, error) {
	db, err := sqlite.New(
		path.Join(dataDir, sqliteFilename),
		auditlog.FS,
		dataSourceOptions,
	)
	if err != nil {
		return nil, err
	}
	return NewSQLiteProvider(db), nil
}

func tenantOf(ctx context.Context) (tenant.ID, error) {
	id, err := tenant.FromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("auditlog: %w", err)
	}
	return id, nil
}

func (p *SQLiteProvider) Save(ctx context.Context, e *Entry) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	e.TenantID = tid
	_, err = p.db.NamedExecContext(
		ctx,
		`INSERT INTO auditlog (
			tenant_id,
			timestamp,
			username,
			remote_ip,
			application,
			action,
			affected_id,
			client_id,
			client_hostname,
			request,
			response
		) VALUES (
			:tenant_id,
			:timestamp,
			:username,
			:remote_ip,
			:application,
			:action,
			:affected_id,
			:client_id,
			:client_hostname,
			:request,
			:response
		)`,
		e,
	)

	return err
}

func (p *SQLiteProvider) List(ctx context.Context, options *query.ListOptions) ([]*Entry, error) {
	values := []*Entry{}
	tid, err := tenantOf(ctx)
	if err != nil {
		return values, err
	}

	q := "SELECT * FROM `auditlog` WHERE `tenant_id` = ?"

	q, params := p.converter.AppendOptionsToQuery(options, q, []interface{}{tid})

	err = p.db.SelectContext(ctx, &values, q, params...)
	if err != nil {
		return values, err
	}

	return values, nil
}

func (p *SQLiteProvider) Count(ctx context.Context, options *query.ListOptions) (int, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return 0, err
	}
	var result int

	q := "SELECT COUNT(*) FROM `auditlog` WHERE `tenant_id` = ?"
	countOptions := *options
	countOptions.Pagination = nil
	q, params := p.converter.AppendOptionsToQuery(&countOptions, q, []interface{}{tid})

	err = p.db.GetContext(ctx, &result, q, params...)
	if err != nil {
		return 0, err
	}

	return result, nil
}

// OldestTimestamp returns the oldest entry of any tenant: rotation works on the whole file.
func (p *SQLiteProvider) OldestTimestamp(ctx context.Context) (time.Time, error) {
	var ts time.Time
	q := "SELECT timestamp FROM auditlog ORDER BY timestamp ASC LIMIT 1"
	err := p.db.GetContext(ctx, &ts, q)
	if err != nil {
		return ts, err
	}
	return ts, nil
}

func (p *SQLiteProvider) Close() error {
	return p.db.Close()
}
