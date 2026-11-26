// Package clients stores the OSS server's clients (clients.ClientStore) in Postgres.
package clients

import (
	"context"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/server/clients"
	"github.com/pohcee/procy-server/server/clients/clientdata"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/tenant"

	"github.com/pohcee/procy-server/server/storage/postgres"
)

// Store is a clients.ClientStore on the clients table. Every method is scoped to the tenant carried by its context.
// It reads the clients through the client_records view, which overlays the state device processes report
// (client_state) on the stored records; deleting a client deletes its reported state too.
type Store struct {
	db                      *sqlx.DB
	keepDisconnectedClients *time.Duration
}

var (
	_ clients.ClientStore  = (*Store)(nil)
	_ clients.ClientGetter = (*Store)(nil)
)

// New returns a client store. keepDisconnectedClients has the meaning of clients.NewSQLiteStore's parameter: nil
// keeps disconnected clients forever; otherwise clients disconnected for longer (measured back from
// clientdata.Now) are left out of GetAll and removed by DeleteObsolete.
func New(db *sqlx.DB, keepDisconnectedClients *time.Duration) *Store {
	return &Store{db: db, keepDisconnectedClients: keepDisconnectedClients}
}

// liveSession (queries on client_records alias it "clients") is true for a clients row whose client has a live session in the cluster registry (migration 1200):
// it is connected to some node, whatever disconnected_at says (the node that held an older session may have written
// it after the newer one connected), so it is neither obsolete nor purged.
const liveSession = `EXISTS (SELECT 1 FROM client_sessions cs
	WHERE cs.tenant_id = clients.tenant_id AND cs.client_id = clients.id AND cs.lease_until > now())`

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
	q := "SELECT id, client_auth_id, disconnected_at, details FROM client_records AS clients WHERE tenant_id = $1"
	args := []any{string(tid)}
	if s.keepDisconnectedClients != nil {
		q += " AND (disconnected_at IS NULL OR disconnected_at >= $2 OR " + liveSession + ")"
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

// Get returns the client with the id, or nil when there is none. Like GetAll, it leaves out a client disconnected
// for longer than keepDisconnectedClients.
func (s *Store) Get(ctx context.Context, id string, l *logger.Logger) (*clientdata.Client, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	q := "SELECT id, client_auth_id, disconnected_at, details FROM client_records AS clients WHERE tenant_id = $1 AND id = $2"
	args := []any{string(tid), id}
	if s.keepDisconnectedClients != nil {
		q += " AND (disconnected_at IS NULL OR disconnected_at >= $3)"
		args = append(args, s.cutoff())
	}
	var rows []row
	err = postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &rows, q, args...)
	})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]
	return clients.DecodeStoredClient(clients.StoredClient{
		ID: r.ID, ClientAuthID: r.ClientAuthID, DisconnectedAt: r.DisconnectedAt, Details: r.Details,
	}, l)
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
		// a node saving the client as disconnected while it has a live session elsewhere is the node that held an
		// older session (it ended its own session first): the newer session's record stays
		res, err := tx.ExecContext(ctx, `INSERT INTO clients (tenant_id, id, client_auth_id, disconnected_at, details)
			SELECT $1, $2, $3, $4, $5
			WHERE $4::timestamptz IS NULL OR NOT EXISTS (SELECT 1 FROM client_sessions cs
				WHERE cs.tenant_id = $1 AND cs.client_id = $2 AND cs.lease_until > now())
			ON CONFLICT (tenant_id, id) DO UPDATE SET
				client_auth_id = EXCLUDED.client_auth_id,
				disconnected_at = EXCLUDED.disconnected_at,
				details = EXCLUDED.details`,
			string(tid), sc.ID, sc.ClientAuthID, sc.DisconnectedAt, sc.Details)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err // not written: the newer session's record, and what it reported, stay
		}
		return dropReportedState(ctx, tx, tid, sc.ID)
	})
}

// dropReportedState deletes the state a device process reported for client id: a node that holds the client's
// connection (role "all") just saved the whole record, which must not be hidden by an older report.
func dropReportedState(ctx context.Context, tx *sqlx.Tx, tid tenant.ID, id string) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM client_state WHERE tenant_id = $1 AND client_id = $2", string(tid), id)
	return err
}

// MarkLostSessionsDisconnected marks the clients of the tenant of ctx disconnected (now) that are stored as
// connected but whose session in the cluster registry ended more than two minutes ago or never existed: their node
// stopped without saying so, or lost the database. It returns how many it marked.
func (s *Store) MarkLostSessionsDisconnected(ctx context.Context) (int64, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	err = postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		// a lease lapsed for less than two minutes is left alone: its node may be renewing it right now (after a
		// database outage); renewing also stores the client connected again if the sweep was first
		// the same for what device processes reported (client_state)
		for _, q := range []string{`UPDATE clients SET disconnected_at = now()
			WHERE tenant_id = $1 AND disconnected_at IS NULL AND NOT EXISTS (SELECT 1 FROM client_sessions cs
				WHERE cs.tenant_id = clients.tenant_id AND cs.client_id = clients.id
				AND cs.lease_until > now() - interval '2 minutes')`, `UPDATE client_state SET disconnected_at = now()
			WHERE tenant_id = $1 AND disconnected_at IS NULL AND NOT EXISTS (SELECT 1 FROM client_sessions cs
				WHERE cs.tenant_id = client_state.tenant_id AND cs.client_id = client_state.client_id
				AND cs.lease_until > now() - interval '2 minutes')`} {
			res, err := tx.ExecContext(ctx, q, string(tid))
			if err != nil {
				return err
			}
			m, err := res.RowsAffected()
			if err != nil {
				return err
			}
			n += m
		}
		return nil
	})
	return n, err
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
		// obsolete as the API sees the client (client_records) and without a live session, both rows of it
		var ids []string
		if err := tx.SelectContext(ctx, &ids, "SELECT id FROM client_records AS clients WHERE tenant_id = $1 AND disconnected_at IS NOT NULL AND disconnected_at < $2 AND NOT "+liveSession,
			string(tid), cutoff); err != nil {
			return err
		}
		for _, tbl := range []string{"client_state WHERE tenant_id = $1 AND client_id", "clients WHERE tenant_id = $1 AND id"} {
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+tbl+" = ANY($2)", string(tid), ids); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) Delete(ctx context.Context, id string, _ *logger.Logger) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	return postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM client_state WHERE tenant_id = $1 AND client_id = $2", string(tid), id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM clients WHERE tenant_id = $1 AND id = $2", string(tid), id)
		return err
	})
}

// Close does nothing: the database is shared and closed by its owner.
func (s *Store) Close() error { return nil }

func (s *Store) cutoff() time.Time {
	return clientdata.Now().Add(-*s.keepDisconnectedClients)
}

var (
	_ clients.AdminFieldsSaver = (*Store)(nil)
	_ clients.ACLKeepingSaver  = (*Store)(nil)
)

// errClientGone is returned by SaveAdminFields for a client that is no longer stored.
var errClientGone = errors.New("client not found: it was deleted")

// lockedRow returns the stored client id of tid, locked FOR UPDATE until tx ends, or nil.
func lockedRow(ctx context.Context, tx *sqlx.Tx, tid tenant.ID, id string) (*clientdata.Client, error) {
	var rows []row
	if err := tx.SelectContext(ctx, &rows, `SELECT id, client_auth_id, disconnected_at, details FROM clients
		WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, string(tid), id); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]
	return clients.DecodeStoredClient(clients.StoredClient{
		ID: r.ID, ClientAuthID: r.ClientAuthID, DisconnectedAt: r.DisconnectedAt, Details: r.Details,
	}, nil)
}

// reportedRow returns the client id of tid as client_records shows it (with the state a device process reported),
// or nil.
func reportedRow(ctx context.Context, tx *sqlx.Tx, tid tenant.ID, id string) (*clientdata.Client, error) {
	var rows []row
	if err := tx.SelectContext(ctx, &rows, `SELECT id, client_auth_id, disconnected_at, details FROM client_records
		WHERE tenant_id = $1 AND id = $2`, string(tid), id); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]
	return clients.DecodeStoredClient(clients.StoredClient{
		ID: r.ID, ClientAuthID: r.ClientAuthID, DisconnectedAt: r.DisconnectedAt, Details: r.Details,
	}, nil)
}

func upsert(ctx context.Context, tx *sqlx.Tx, tid tenant.ID, c *clientdata.Client) error {
	sc, err := clients.EncodeStoredClient(c)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO clients (tenant_id, id, client_auth_id, disconnected_at, details)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, id) DO UPDATE SET
			client_auth_id = EXCLUDED.client_auth_id,
			disconnected_at = EXCLUDED.disconnected_at,
			details = EXCLUDED.details`,
		string(tid), sc.ID, sc.ClientAuthID, sc.DisconnectedAt, sc.Details)
	return err
}

// SaveAdminFields writes the allowed user groups of c into its stored record and leaves the rest as stored: a node
// that holds no client connections (role api) must not write back the connection state or the reported fields it
// has cached. A client deleted meanwhile is not brought back.
func (s *Store) SaveAdminFields(ctx context.Context, c *clientdata.Client) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	groups := c.GetAllowedUserGroups()
	return postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		stored, err := lockedRow(ctx, tx, tid, c.GetID())
		if err != nil {
			return err
		}
		if stored == nil {
			// a client only a device process reported (client_state) gets its clients row now
			if stored, err = reportedRow(ctx, tx, tid, c.GetID()); err != nil {
				return err
			}
		}
		if stored == nil {
			return errClientGone
		}
		stored.SetAllowedUserGroups(groups)
		return upsert(ctx, tx, tid, stored)
	})
}

// SaveKeepingACL saves c with the allowed user groups as stored (and sets them on c): a save of what a client
// reported must not undo an ACL change another node made since this node cached the client. Like Save, it replaces
// what a device process reported about c.
func (s *Store) SaveKeepingACL(ctx context.Context, c *clientdata.Client) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	return postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		stored, err := lockedRow(ctx, tx, tid, c.GetID())
		if err != nil {
			return err
		}
		if stored != nil {
			c.SetAllowedUserGroups(stored.GetAllowedUserGroups())
		}
		if err := upsert(ctx, tx, tid, c); err != nil {
			return err
		}
		return dropReportedState(ctx, tx, tid, c.GetID())
	})
}
