package cgroups

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
)

type ClientGroupProvider interface {
	Get(ctx context.Context, id string) (*ClientGroup, error)
	GetAll(ctx context.Context) ([]*ClientGroup, error)
	List(ctx context.Context, options *query.ListOptions) ([]*ClientGroup, error)
	Create(ctx context.Context, group *ClientGroup) error
	Update(ctx context.Context, group *ClientGroup) error
	Delete(ctx context.Context, id string) error
	Close() error
}

// SqliteProvider stores client groups. Every method is scoped to the tenant carried by its context
// and fails with tenant.ErrMissing when there is none.
type SqliteProvider struct {
	db        *sqlx.DB
	converter *query.SQLConverter
}

func NewSqliteProvider(db *sqlx.DB) (*SqliteProvider, error) {
	return &SqliteProvider{
		db:        db,
		converter: query.NewSQLConverter(db.DriverName()),
	}, nil
}

func tenantOf(ctx context.Context) (tenant.ID, error) {
	id, err := tenant.FromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("client groups: %w", err)
	}
	return id, nil
}

func (p *SqliteProvider) GetAll(ctx context.Context) ([]*ClientGroup, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var res []*ClientGroup
	err = p.db.SelectContext(
		ctx,
		&res,
		"SELECT * FROM client_groups WHERE tenant_id = ? ORDER BY id COLLATE NOCASE",
		tid,
	)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (p *SqliteProvider) List(ctx context.Context, options *query.ListOptions) ([]*ClientGroup, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	q, params := p.converter.ConvertListOptionsToQuery(options, "SELECT * FROM client_groups WHERE tenant_id = ?")
	params = append([]interface{}{tid}, params...)

	var res []*ClientGroup
	err = p.db.SelectContext(ctx, &res, q, params...)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (p *SqliteProvider) Get(ctx context.Context, id string) (*ClientGroup, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	res := &ClientGroup{}
	err = p.db.GetContext(ctx, res, "SELECT * FROM client_groups WHERE tenant_id = ? AND id = ?", tid, id)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return res, nil
}

func (p *SqliteProvider) Create(ctx context.Context, group *ClientGroup) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	group.TenantID = tid
	_, err = p.db.NamedExecContext(
		ctx,
		"INSERT INTO client_groups (tenant_id, id, description, params, allowed_user_groups) VALUES (:tenant_id, :id, :description, :params, :allowed_user_groups)",
		group,
	)
	return err
}

func (p *SqliteProvider) Update(ctx context.Context, group *ClientGroup) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	group.TenantID = tid
	_, err = p.db.NamedExecContext(
		ctx,
		"INSERT OR REPLACE INTO client_groups (tenant_id, id, description, params, allowed_user_groups) VALUES (:tenant_id, :id, :description, :params, :allowed_user_groups)",
		group,
	)
	return err
}

func (p *SqliteProvider) Delete(ctx context.Context, id string) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	_, err = p.db.ExecContext(ctx, "DELETE FROM client_groups WHERE tenant_id = ? AND id = ?", tid, id)
	return err
}

func (p *SqliteProvider) Close() error {
	return p.db.Close()
}
