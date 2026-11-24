// Package clients stores the OSS server's clients (clients.ClientStore) in Postgres.
package clients

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/share/logger"

	"github.com/openrport/openrport/server/storage/postgres"
)

// Store is a clients.ClientStore on the clients table. Every method is scoped to the tenant carried by its context.
type Store struct {
	db                      *sqlx.DB
	keepDisconnectedClients *time.Duration
}

var _ clients.ClientStore = (*Store)(nil)

// New returns a client store. keepDisconnectedClients has the meaning of clients.NewSQLiteStore's parameter: nil
// keeps disconnected clients forever; otherwise clients disconnected for longer (measured back from
// clientdata.Now) are left out of GetAll and removed by DeleteObsolete.
func New(db *sqlx.DB, keepDisconnectedClients *time.Duration) *Store {
	return &Store{db: db, keepDisconnectedClients: keepDisconnectedClients}
}

type row struct {
	ID             string     `db:"id"`
	ClientAuthID   string     `db:"client_auth_id"`
	DisconnectedAt *time.Time `db:"disconnected_at"`
	Details        string     `db:"details"`
}

func (s *Store) GetAll(ctx context.Context, l *logger.Logger) ([]*clientdata.Client, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	q := "SELECT id, client_auth_id, disconnected_at, details FROM clients WHERE tenant_id = $1"
	args := []any{string(tid)}
	if s.keepDisconnectedClients != nil {
		q += " AND (disconnected_at IS NULL OR disconnected_at >= $2)"
		args = append(args, s.cutoff())
	}
	var rows []row
	err = postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &rows, q, args...)
	})
	if err != nil {
		return nil, err
	}
	res := make([]*clientdata.Client, 0, len(rows))
	for _, r := range rows {
		c, err := clients.DecodeStoredClient(clients.StoredClient{
			ID: r.ID, ClientAuthID: r.ClientAuthID, DisconnectedAt: r.DisconnectedAt, Details: r.Details,
		}, l)
		if err != nil {
			return nil, err
		}
		res = append(res, c)
	}
	return res, nil
}

func (s *Store) Save(ctx context.Context, c *clientdata.Client) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	sc, err := clients.EncodeStoredClient(c)
	if err != nil {
		return err
	}
	return postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO clients (tenant_id, id, client_auth_id, disconnected_at, details)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, id) DO UPDATE SET
				client_auth_id = EXCLUDED.client_auth_id,
				disconnected_at = EXCLUDED.disconnected_at,
				details = EXCLUDED.details`,
			string(tid), sc.ID, sc.ClientAuthID, sc.DisconnectedAt, sc.Details)
		return err
	})
}

func (s *Store) DeleteObsolete(ctx context.Context, _ *logger.Logger) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	if s.keepDisconnectedClients == nil {
		return nil
	}
	cutoff := s.cutoff()
	return postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx,
			"DELETE FROM clients WHERE tenant_id = $1 AND disconnected_at IS NOT NULL AND disconnected_at < $2",
			string(tid), cutoff)
		return err
	})
}

func (s *Store) Delete(ctx context.Context, id string, _ *logger.Logger) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	return postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM clients WHERE tenant_id = $1 AND id = $2", string(tid), id)
		return err
	})
}

// Close does nothing: the database is shared and closed by its owner.
func (s *Store) Close() error { return nil }

func (s *Store) cutoff() time.Time {
	return clientdata.Now().Add(-*s.keepDisconnectedClients)
}
