// Package clientgroups stores the OSS server's client groups (cgroups.ClientGroupProvider) in Postgres.
package clientgroups

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/cgroups"
	"github.com/openrport/openrport/share/query"

	"github.com/openrport/openrport/server/storage/postgres"
)

// Store is a cgroups.ClientGroupProvider on the client_groups table. Every method is scoped to the tenant carried by
// its context.
type Store struct {
	db        *sqlx.DB
	converter *query.SQLConverter
}

var _ cgroups.ClientGroupProvider = (*Store)(nil)

// New returns a client group provider.
func New(db *sqlx.DB) *Store {
	return &Store{db: db, converter: query.NewSQLConverter("postgres")}
}

const columns = "tenant_id, id, description, params, allowed_user_groups"

// GetAll returns the tenant's groups ordered by id ignoring ASCII case, as SQLite's COLLATE NOCASE does.
func (s *Store) GetAll(ctx context.Context) ([]*cgroups.ClientGroup, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var res []*cgroups.ClientGroup
	err = postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &res,
			"SELECT "+columns+" FROM client_groups WHERE tenant_id = $1 ORDER BY lower(id), id", string(tid))
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Store) List(ctx context.Context, options *query.ListOptions) ([]*cgroups.ClientGroup, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	q, params := s.converter.AppendOptionsToQuery(options, "SELECT * FROM client_groups WHERE tenant_id = ?", []any{string(tid)})
	var res []*cgroups.ClientGroup
	err = postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &res, tx.Rebind(q), params...)
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Get returns the group, or nil when the tenant has none with the id.
func (s *Store) Get(ctx context.Context, id string) (*cgroups.ClientGroup, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	res := &cgroups.ClientGroup{}
	err = postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		return tx.GetContext(ctx, res, "SELECT "+columns+" FROM client_groups WHERE tenant_id = $1 AND id = $2", string(tid), id)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Create adds a group; it fails when the tenant already has one with the id.
func (s *Store) Create(ctx context.Context, group *cgroups.ClientGroup) error {
	return s.write(ctx, group, "INSERT INTO client_groups ("+columns+") VALUES (:tenant_id, :id, :description, :params, :allowed_user_groups)")
}

// Update replaces the group with the id, or adds it when there is none (as the SQLite INSERT OR REPLACE does).
func (s *Store) Update(ctx context.Context, group *cgroups.ClientGroup) error {
	return s.write(ctx, group, "INSERT INTO client_groups ("+columns+") VALUES (:tenant_id, :id, :description, :params, :allowed_user_groups)"+
		" ON CONFLICT (tenant_id, id) DO UPDATE SET description = EXCLUDED.description, params = EXCLUDED.params,"+
		" allowed_user_groups = EXCLUDED.allowed_user_groups")
}

func (s *Store) write(ctx context.Context, group *cgroups.ClientGroup, q string) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	group.TenantID = tid
	return postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		_, err := tx.NamedExecContext(ctx, q, group)
		return err
	})
}

func (s *Store) Delete(ctx context.Context, id string) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	return postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM client_groups WHERE tenant_id = $1 AND id = $2", string(tid), id)
		return err
	})
}

// Close does nothing: the database is shared and closed by its owner.
func (s *Store) Close() error { return nil }
