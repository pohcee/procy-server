// Package clientsessions is the cluster's session registry on Postgres (migration 0450): which device process holds
// each client's connection, and where that process listens for peer calls. Device processes write it only through
// the registry functions (migrate.RegistryFunctions); api processes read it to locate a client.
package clientsessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/peer"
	"github.com/openrport/openrport/server/storage/postgres"
)

// Store is the registry on db.
type Store struct {
	db *sqlx.DB
}

var (
	_ peer.SessionStore = (*Store)(nil)
	_ peer.Locator      = (*Store)(nil)
)

// New returns a Store on db (migrated).
func New(db *sqlx.DB) *Store { return &Store{db: db} }

func leaseSeconds(d time.Duration) int {
	s := int(d / time.Second)
	if s < 1 {
		s = 1
	}
	return s
}

// Heartbeat records the node's region and peer address and that it is up.
func (s *Store) Heartbeat(ctx context.Context, node peer.NodeInfo) error {
	_, err := s.db.ExecContext(ctx, "SELECT rport_node_heartbeat($1, $2, $3)", node.NodeID, node.Region, node.PeerAddress)
	if err != nil {
		return fmt.Errorf("registry: heartbeat: %w", err)
	}
	return nil
}

// Register records a new session of a client on nodeID, replacing any other session of the client.
func (s *Store) Register(ctx context.Context, nodeID string, sess peer.Session, lease time.Duration) error {
	_, err := s.db.ExecContext(ctx, "SELECT rport_register_session($1, $2, $3, $4, $5)",
		string(sess.Tenant), sess.ClientID, nodeID, sess.SessionID, leaseSeconds(lease))
	if err != nil {
		return fmt.Errorf("registry: register session of client %q: %w", sess.ClientID, err)
	}
	return nil
}

type heldSession struct {
	TenantID  string `json:"tenant_id"`
	ClientID  string `json:"client_id"`
	SessionID string `json:"session_id"`
}

// Renew extends the leases of the sessions held by nodeID and returns the ids of those another session replaced.
func (s *Store) Renew(ctx context.Context, nodeID string, held []peer.Session, lease time.Duration) ([]string, error) {
	in := make([]heldSession, 0, len(held))
	for _, h := range held {
		in = append(in, heldSession{TenantID: string(h.Tenant), ClientID: h.ClientID, SessionID: h.SessionID})
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var lost []string
	if err := s.db.SelectContext(ctx, &lost, "SELECT * FROM rport_renew_sessions($1, $2::jsonb, $3)", nodeID, string(b), leaseSeconds(lease)); err != nil {
		return nil, fmt.Errorf("registry: renew sessions: %w", err)
	}
	return lost, nil
}

// End removes the session sessionID.
func (s *Store) End(ctx context.Context, sessionID string) error {
	if _, err := s.db.ExecContext(ctx, "SELECT rport_end_session($1)", sessionID); err != nil {
		return fmt.Errorf("registry: end session: %w", err)
	}
	return nil
}

// Locate returns the node holding the live session of clientID in the tenant of ctx, or nil when there is none.
func (s *Store) Locate(ctx context.Context, clientID string) (*peer.Location, error) {
	var loc peer.Location
	err := postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		return tx.QueryRowxContext(ctx, `SELECT s.node_id, n.peer_address
			FROM client_sessions s JOIN nodes n ON n.node_id = s.node_id
			WHERE s.tenant_id = current_setting('rport.tenant_id', true) AND s.client_id = $1 AND s.lease_until > now()`,
			clientID).Scan(&loc.NodeID, &loc.PeerAddress)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("registry: locate client %q: %w", clientID, err)
	}
	return &loc, nil
}
