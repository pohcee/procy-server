package storedtunnels

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/tenant"
)

// SQLiteProvider stores tunnels saved for a client. Every method is scoped to the tenant carried by its
// context and fails with tenant.ErrMissing when there is none.
type SQLiteProvider struct {
	db        *sqlx.DB
	converter *query.SQLConverter
}

var _ Provider = (*SQLiteProvider)(nil)

// NewSQLiteProvider returns a stored tunnel provider on an opened, migrated clients database.
func NewSQLiteProvider(db *sqlx.DB) *SQLiteProvider {
	return &SQLiteProvider{
		db:        db,
		converter: query.NewSQLConverter(db.DriverName()),
	}
}

func tenantOf(ctx context.Context) (tenant.ID, error) {
	id, err := tenant.FromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("stored tunnels: %w", err)
	}
	return id, nil
}

func (p *SQLiteProvider) Insert(ctx context.Context, t *StoredTunnel) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	t.TenantID = tid
	_, err = p.db.NamedExecContext(ctx,
		`INSERT INTO stored_tunnels (
			tenant_id,
			id,
			client_id,
			created_at,
			name,
			scheme,
			remote_ip,
			remote_port,
			public_port,
			acl,
			further_options
		) VALUES (
			:tenant_id,
			:id,
			:client_id,
			:created_at,
			:name,
			:scheme,
			:remote_ip,
			:remote_port,
			:public_port,
			:acl,
			:further_options
		)`,
		t,
	)

	return err
}

func (p *SQLiteProvider) Update(ctx context.Context, t *StoredTunnel) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	t.TenantID = tid
	_, err = p.db.NamedExecContext(ctx,
		`UPDATE stored_tunnels SET
			name = :name,
			scheme = :scheme,
			remote_ip = :remote_ip,
			remote_port = :remote_port,
			public_port = :public_port,
			acl = :acl,
			further_options = :further_options
		WHERE tenant_id = :tenant_id AND client_id = :client_id AND id = :id`,
		t,
	)

	return err
}

func (p *SQLiteProvider) List(ctx context.Context, clientID string, options *query.ListOptions) ([]*StoredTunnel, error) {
	values := []*StoredTunnel{}

	tid, err := tenantOf(ctx)
	if err != nil {
		return values, err
	}

	q := "SELECT * FROM stored_tunnels WHERE tenant_id = ? AND client_id = ?"
	params := []interface{}{tid, clientID}

	q, params = p.converter.AppendOptionsToQuery(options, q, params)

	err = p.db.SelectContext(ctx, &values, q, params...)
	if err != nil {
		return values, err
	}

	return values, nil
}

func (p *SQLiteProvider) Count(ctx context.Context, clientID string, options *query.ListOptions) (int, error) {
	var result int

	tid, err := tenantOf(ctx)
	if err != nil {
		return 0, err
	}

	q := "SELECT COUNT(*) FROM stored_tunnels WHERE tenant_id = ? AND client_id = ?"
	params := []interface{}{tid, clientID}

	countOptions := *options
	countOptions.Pagination = nil
	q, params = p.converter.AppendOptionsToQuery(&countOptions, q, params)

	err = p.db.GetContext(ctx, &result, q, params...)
	if err != nil {
		return 0, err
	}

	return result, nil
}

func (p *SQLiteProvider) Delete(ctx context.Context, clientID, id string) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	_, err = p.db.ExecContext(ctx, "DELETE FROM stored_tunnels WHERE tenant_id = ? AND client_id = ? AND id = ?", tid, clientID, id)
	return err
}
