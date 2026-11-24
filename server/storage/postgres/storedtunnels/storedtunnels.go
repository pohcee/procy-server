// Package storedtunnels stores the tunnels saved for clients (storedtunnels.Provider) in Postgres.
package storedtunnels

import (
	"context"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/clients/storedtunnels"
	"github.com/openrport/openrport/share/query"

	"github.com/openrport/openrport/server/storage/postgres"
)

// Store is a storedtunnels.Provider on the stored_tunnels table. Every method is scoped to the tenant carried by its
// context. A tunnel belongs to a client of the clients table (internal/pg/clients); deleting the client deletes its
// tunnels.
type Store struct {
	db        *sqlx.DB
	converter *query.SQLConverter
}

var _ storedtunnels.Provider = (*Store)(nil)

// New returns a stored tunnel provider.
func New(db *sqlx.DB) *Store {
	return &Store{db: db, converter: query.NewSQLConverter("postgres")}
}

func (s *Store) Insert(ctx context.Context, t *storedtunnels.StoredTunnel) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	t.TenantID = tid
	return postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		_, err := tx.NamedExecContext(ctx, `INSERT INTO stored_tunnels (
				tenant_id, id, client_id, created_at, name, scheme, remote_ip, remote_port, public_port, acl, further_options
			) VALUES (
				:tenant_id, :id, :client_id, :created_at, :name, :scheme, :remote_ip, :remote_port, :public_port, :acl, :further_options
			)`, t)
		return err
	})
}

func (s *Store) Update(ctx context.Context, t *storedtunnels.StoredTunnel) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	t.TenantID = tid
	return postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		_, err := tx.NamedExecContext(ctx, `UPDATE stored_tunnels SET
				name = :name,
				scheme = :scheme,
				remote_ip = :remote_ip,
				remote_port = :remote_port,
				public_port = :public_port,
				acl = :acl,
				further_options = :further_options
			WHERE tenant_id = :tenant_id AND client_id = :client_id AND id = :id`, t)
		return err
	})
}

func (s *Store) List(ctx context.Context, clientID string, options *query.ListOptions) ([]*storedtunnels.StoredTunnel, error) {
	values := []*storedtunnels.StoredTunnel{}
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return values, err
	}
	q, params := s.converter.AppendOptionsToQuery(options,
		"SELECT * FROM stored_tunnels WHERE tenant_id = ? AND client_id = ?", []any{string(tid), clientID})
	err = postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &values, tx.Rebind(q), params...)
	})
	return values, err
}

func (s *Store) Count(ctx context.Context, clientID string, options *query.ListOptions) (int, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return 0, err
	}
	countOptions := query.ListOptions{}
	if options != nil {
		countOptions = *options
	}
	countOptions.Pagination = nil
	q, params := s.converter.AppendOptionsToQuery(&countOptions,
		"SELECT COUNT(*) FROM stored_tunnels WHERE tenant_id = ? AND client_id = ?", []any{string(tid), clientID})
	var n int
	err = postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		return tx.GetContext(ctx, &n, tx.Rebind(q), params...)
	})
	return n, err
}

func (s *Store) Delete(ctx context.Context, clientID, id string) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	return postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx,
			"DELETE FROM stored_tunnels WHERE tenant_id = $1 AND client_id = $2 AND id = $3", string(tid), clientID, id)
		return err
	})
}
