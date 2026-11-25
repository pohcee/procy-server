// Package oidc is the Postgres storage of OpenID Connect sign-in (migration 1700): pending sign-ins and sign-in
// codes (shared by every node), the identities pinned to accounts and the sign-in policy.
package oidc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/oidc"
	"github.com/openrport/openrport/server/storage/postgres"
)

// Store is the Postgres oidc.Store and oidc.Policies, on the application connection.
type Store struct {
	db *sqlx.DB
}

var (
	_ oidc.Store    = (*Store)(nil)
	_ oidc.Policies = (*Store)(nil)
)

// New returns the store on db.
func New(db *sqlx.DB) *Store { return &Store{db: db} }

// SaveLogin implements oidc.Store. It deletes expired sign-ins and codes on the way.
func (s *Store) SaveLogin(ctx context.Context, l oidc.Login) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM oidc_logins WHERE expires_at < now()"); err != nil {
		return fmt.Errorf("oidc: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM oidc_tickets WHERE expires_at < now() - interval '1 hour'"); err != nil {
		return fmt.Errorf("oidc: %w", err)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO oidc_logins (state_hash, nonce, verifier, binding_hash, return_to, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, l.StateHash, l.Nonce, l.Verifier, l.BindingHash, l.ReturnTo, l.ExpiresAt)
	if err != nil {
		return fmt.Errorf("oidc: save sign-in: %w", err)
	}
	return nil
}

// TakeLogin implements oidc.Store.
func (s *Store) TakeLogin(ctx context.Context, stateHash string, now time.Time) (*oidc.Login, error) {
	l := &oidc.Login{}
	err := s.db.GetContext(ctx, l, `DELETE FROM oidc_logins WHERE state_hash = $1
		RETURNING state_hash, nonce, verifier, binding_hash, return_to, expires_at`, stateHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, oidc.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("oidc: take sign-in: %w", err)
	}
	if !l.ExpiresAt.After(now) {
		return nil, oidc.ErrNotFound
	}
	return l, nil
}

// SaveTicket implements oidc.Store.
func (s *Store) SaveTicket(ctx context.Context, t oidc.Ticket) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO oidc_tickets (code_hash, username, expires_at) VALUES ($1, $2, $3)",
		t.CodeHash, t.Username, t.ExpiresAt)
	if err != nil {
		return fmt.Errorf("oidc: save sign-in code: %w", err)
	}
	return nil
}

// GetTicket implements oidc.Store.
func (s *Store) GetTicket(ctx context.Context, codeHash string, now time.Time) (*oidc.Ticket, error) {
	var row struct {
		oidc.Ticket
		UsedAt *time.Time `db:"used_at"`
	}
	err := s.db.GetContext(ctx, &row, "SELECT code_hash, username, expires_at, used_at FROM oidc_tickets WHERE code_hash = $1", codeHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, oidc.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("oidc: sign-in code: %w", err)
	}
	if row.UsedAt != nil || !row.ExpiresAt.After(now) {
		return nil, oidc.ErrExpired
	}
	return &row.Ticket, nil
}

// UseTicket implements oidc.Store.
func (s *Store) UseTicket(ctx context.Context, codeHash string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, "UPDATE oidc_tickets SET used_at = $2 WHERE code_hash = $1 AND used_at IS NULL AND expires_at > $2",
		codeHash, now)
	if err != nil {
		return fmt.Errorf("oidc: use sign-in code: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return oidc.ErrExpired
	}
	return nil
}

// Resolve implements oidc.Store (rport_oidc_resolve).
func (s *Store) Resolve(ctx context.Context, id oidc.Identity) (string, error) {
	var out struct {
		Login   sql.NullString `db:"login"`
		Outcome string         `db:"outcome"`
	}
	if err := s.db.GetContext(ctx, &out, "SELECT login, outcome FROM rport_oidc_resolve($1, $2, $3)", id.Issuer, id.Subject, id.Email); err != nil {
		return "", fmt.Errorf("oidc: resolve identity: %w", err)
	}
	switch out.Outcome {
	case "pinned", "linked":
		return out.Login.String, nil
	case "ambiguous":
		return "", oidc.ErrAmbiguous
	case "mismatch":
		return "", oidc.ErrMismatch
	}
	return "", oidc.ErrUnknownAccount
}

// Linked implements oidc.Store.
func (s *Store) Linked(ctx context.Context, usernames []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(usernames) == 0 {
		return out, nil
	}
	var linked []string
	if err := s.db.SelectContext(ctx, &linked, "SELECT DISTINCT username FROM account_identities WHERE username = ANY($1)", usernames); err != nil {
		return nil, fmt.Errorf("oidc: identities: %w", err)
	}
	for _, u := range linked {
		out[u] = true
	}
	return out, nil
}

// Unlink implements oidc.Store: username must be a member of the tenant of ctx.
func (s *Store) Unlink(ctx context.Context, username string) error {
	return postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		var ok bool
		if err := tx.GetContext(ctx, &ok, "SELECT rport_oidc_unlink($1)", username); err != nil {
			return fmt.Errorf("oidc: unlink: %w", err)
		}
		if !ok {
			return oidc.ErrNoSuchUser
		}
		return nil
	})
}

// Policy implements oidc.Policies.
func (s *Store) Policy(ctx context.Context, username string) (oidc.Policy, error) {
	var p struct {
		Require bool   `db:"require_oidc"`
		Method  string `db:"sign_in"`
	}
	err := postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		tid, err := postgres.TenantOf(ctx)
		if err != nil {
			return err
		}
		return tx.GetContext(ctx, &p, `SELECT t.require_oidc,
				coalesce((SELECT u.sign_in FROM users u WHERE u.tenant_id = t.id AND u.username = $2), 'both') AS sign_in
			FROM tenants t WHERE t.id = $1`, string(tid), username)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return oidc.Policy{}, nil
	}
	if err != nil {
		return oidc.Policy{}, fmt.Errorf("oidc: sign-in policy: %w", err)
	}
	return oidc.Policy{RequireOIDC: p.Require, Method: oidc.Method(p.Method)}, nil
}

// RequireOIDC implements oidc.Policies.
func (s *Store) RequireOIDC(ctx context.Context) (bool, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return false, err
	}
	var require bool
	err = s.db.GetContext(ctx, &require, "SELECT require_oidc FROM tenants WHERE id = $1", string(tid))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("oidc: sign-in policy: %w", err)
	}
	return require, nil
}

// SetRequireOIDC implements oidc.Policies.
func (s *Store) SetRequireOIDC(ctx context.Context, require bool) error {
	return postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		tid, err := postgres.TenantOf(ctx)
		if err != nil {
			return err
		}
		var found bool
		if err := tx.GetContext(ctx, &found, "SELECT rport_set_require_oidc($1, $2)", string(tid), require); err != nil {
			return fmt.Errorf("oidc: set sign-in policy: %w", err)
		}
		if !found {
			return oidc.ErrNoSuchUser
		}
		return nil
	})
}

// SetMethod implements oidc.Policies.
func (s *Store) SetMethod(ctx context.Context, username string, m oidc.Method) error {
	return postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		tid, err := postgres.TenantOf(ctx)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "UPDATE users SET sign_in = $3 WHERE tenant_id = $1 AND username = $2", string(tid), username, string(m))
		if err != nil {
			return fmt.Errorf("oidc: set sign-in method: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return oidc.ErrNoSuchUser
		}
		return nil
	})
}

// Methods implements oidc.Policies.
func (s *Store) Methods(ctx context.Context) (map[string]oidc.Method, error) {
	out := map[string]oidc.Method{}
	err := postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		tid, err := postgres.TenantOf(ctx)
		if err != nil {
			return err
		}
		var rows []struct {
			Username string `db:"username"`
			Method   string `db:"sign_in"`
		}
		if err := tx.SelectContext(ctx, &rows, "SELECT username, sign_in FROM users WHERE tenant_id = $1", string(tid)); err != nil {
			return err
		}
		for _, r := range rows {
			out[r.Username] = oidc.Method(r.Method)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("oidc: sign-in methods: %w", err)
	}
	return out, nil
}
