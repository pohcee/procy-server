package oidc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// SQLiteStore is the Store of a standalone server (api_sessions.db, migration 004). Logins lists the API users,
// whose usernames the email of a sign-in is matched against.
type SQLiteStore struct {
	db *sqlx.DB
	// mu serializes Resolve: the lookup and the pin are one step
	mu     sync.Mutex
	logins func(ctx context.Context) ([]string, error)
}

var _ Store = (*SQLiteStore)(nil)

// NewSQLiteStore returns the store on db (the api_sessions database).
func NewSQLiteStore(db *sqlx.DB) *SQLiteStore { return &SQLiteStore{db: db} }

// SetLogins sets the list of the API users' usernames.
func (s *SQLiteStore) SetLogins(fn func(ctx context.Context) ([]string, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logins = fn
}

// SaveLogin implements Store.
func (s *SQLiteStore) SaveLogin(ctx context.Context, l Login) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM oidc_logins WHERE expires_at < ?", time.Now().UTC()); err != nil {
		return fmt.Errorf("oidc: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM oidc_tickets WHERE expires_at < ?", time.Now().UTC().Add(-time.Hour)); err != nil {
		return fmt.Errorf("oidc: %w", err)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO oidc_logins (state_hash, nonce, verifier, binding_hash, return_to, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`, l.StateHash, l.Nonce, l.Verifier, l.BindingHash, l.ReturnTo, l.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("oidc: save sign-in: %w", err)
	}
	return nil
}

// TakeLogin implements Store.
func (s *SQLiteStore) TakeLogin(ctx context.Context, stateHash string, now time.Time) (*Login, error) {
	l := &Login{}
	err := s.db.GetContext(ctx, l, `DELETE FROM oidc_logins WHERE state_hash = ?
		RETURNING state_hash, nonce, verifier, binding_hash, return_to, expires_at`, stateHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("oidc: take sign-in: %w", err)
	}
	if !l.ExpiresAt.After(now) {
		return nil, ErrNotFound
	}
	return l, nil
}

// SaveTicket implements Store.
func (s *SQLiteStore) SaveTicket(ctx context.Context, t Ticket) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO oidc_tickets (code_hash, username, expires_at) VALUES (?, ?, ?)",
		t.CodeHash, t.Username, t.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("oidc: save sign-in code: %w", err)
	}
	return nil
}

// GetTicket implements Store.
func (s *SQLiteStore) GetTicket(ctx context.Context, codeHash string, now time.Time) (*Ticket, error) {
	var row struct {
		Ticket
		UsedAt *time.Time `db:"used_at"`
	}
	err := s.db.GetContext(ctx, &row, "SELECT code_hash, username, expires_at, used_at FROM oidc_tickets WHERE code_hash = ?", codeHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("oidc: sign-in code: %w", err)
	}
	if row.UsedAt != nil || !row.ExpiresAt.After(now) {
		return nil, ErrExpired
	}
	return &row.Ticket, nil
}

// UseTicket implements Store.
func (s *SQLiteStore) UseTicket(ctx context.Context, codeHash string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, "UPDATE oidc_tickets SET used_at = ? WHERE code_hash = ? AND used_at IS NULL AND expires_at > ?",
		now.UTC(), codeHash, now.UTC())
	if err != nil {
		return fmt.Errorf("oidc: use sign-in code: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return ErrExpired
	}
	return nil
}

// Resolve implements Store.
func (s *SQLiteStore) Resolve(ctx context.Context, id Identity) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var username string
	err := s.db.GetContext(ctx, &username, "SELECT username FROM oidc_identities WHERE issuer = ? AND subject = ?", id.Issuer, id.Subject)
	switch {
	case err == nil:
		_, err := s.db.ExecContext(ctx, "UPDATE oidc_identities SET last_login_at = ?, email = ? WHERE issuer = ? AND subject = ?",
			time.Now().UTC(), id.Email, id.Issuer, id.Subject)
		if err != nil {
			return "", fmt.Errorf("oidc: %w", err)
		}
		return username, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("oidc: identity: %w", err)
	}
	if s.logins == nil {
		return "", ErrUnknownAccount
	}
	all, err := s.logins(ctx)
	if err != nil {
		return "", err
	}
	username, err = matchEmail(all, id.Email)
	if err != nil {
		return "", err
	}
	var pinned int
	if err := s.db.GetContext(ctx, &pinned, "SELECT count(*) FROM oidc_identities WHERE username = ? AND issuer = ?", username, id.Issuer); err != nil {
		return "", fmt.Errorf("oidc: identity: %w", err)
	}
	if pinned > 0 {
		return "", ErrMismatch
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO oidc_identities (issuer, subject, username, email, linked_at, last_login_at)
		VALUES (?, ?, ?, ?, ?, ?)`, id.Issuer, id.Subject, username, id.Email, time.Now().UTC(), time.Now().UTC())
	if err != nil {
		return "", fmt.Errorf("oidc: pin identity: %w", err)
	}
	return username, nil
}

// matchEmail returns the one login of logins equal to email ignoring case.
func matchEmail(logins []string, email string) (string, error) {
	var found []string
	for _, l := range logins {
		if email != "" && strings.EqualFold(l, email) {
			found = append(found, l)
		}
	}
	switch len(found) {
	case 0:
		return "", ErrUnknownAccount
	case 1:
		return found[0], nil
	}
	return "", ErrAmbiguous
}

// Linked implements Store.
func (s *SQLiteStore) Linked(ctx context.Context, usernames []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(usernames) == 0 {
		return out, nil
	}
	q, args, err := sqlx.In("SELECT DISTINCT username FROM oidc_identities WHERE username IN (?)", usernames)
	if err != nil {
		return nil, err
	}
	var linked []string
	if err := s.db.SelectContext(ctx, &linked, s.db.Rebind(q), args...); err != nil {
		return nil, fmt.Errorf("oidc: identities: %w", err)
	}
	for _, u := range linked {
		out[u] = true
	}
	return out, nil
}

// Unlink implements Store.
func (s *SQLiteStore) Unlink(ctx context.Context, username string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM oidc_identities WHERE username = ?", username); err != nil {
		return fmt.Errorf("oidc: unlink: %w", err)
	}
	return nil
}
