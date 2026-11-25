// Package oidc is OpenID Connect sign-in ("Sign in with Google"): the provider (discovery, the authorization code
// flow with PKCE, ID token verification), the storage of pending sign-ins and of the identities pinned to accounts,
// and the sign-in policy of memberships and organizations. The HTTP handlers live with the other login handlers in
// the server package; the Postgres storage in storage/postgres/oidc, the standalone one (SQLite) here.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Lifetimes of the pending sign-in (from GET /oidc/login to the callback) and of the sign-in code the callback hands
// to the web UI (until POST /oidc/token).
const (
	LoginTTL  = 10 * time.Minute
	TicketTTL = 5 * time.Minute
)

var (
	// ErrNotFound: no such pending sign-in or sign-in code.
	ErrNotFound = errors.New("oidc: not found")
	// ErrExpired: the sign-in code expired or was used already.
	ErrExpired = errors.New("oidc: sign-in code expired or used")
	// ErrUnknownAccount: no account has the identity's email (accounts are never created by a sign-in).
	ErrUnknownAccount = errors.New("oidc: no account with this email")
	// ErrAmbiguous: several logins match the email when case is ignored.
	ErrAmbiguous = errors.New("oidc: several accounts match this email")
	// ErrMismatch: the account matching the email is pinned to a different identity of the same issuer.
	ErrMismatch = errors.New("oidc: the account is linked to a different identity")
	// ErrNoSuchUser: the login is not a member of the organization.
	ErrNoSuchUser = errors.New("oidc: no such user")
)

// Identity is a verified sign-in at the provider.
type Identity struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	// HostedDomain is Google's hd claim (the Workspace domain), "" for none.
	HostedDomain string
}

// Login is a pending sign-in: created by GET /oidc/login, taken (once) by the callback. The state and the browser
// binding are stored as hashes only.
type Login struct {
	StateHash   string    `db:"state_hash"`
	Nonce       string    `db:"nonce"`
	Verifier    string    `db:"verifier"`
	BindingHash string    `db:"binding_hash"`
	ReturnTo    string    `db:"return_to"`
	ExpiresAt   time.Time `db:"expires_at"`
}

// Ticket is a finished sign-in waiting for POST /oidc/token: a one-time code (stored as a hash) naming the login.
type Ticket struct {
	CodeHash  string    `db:"code_hash"`
	Username  string    `db:"username"`
	ExpiresAt time.Time `db:"expires_at"`
}

// Store keeps pending sign-ins, sign-in codes and the identities pinned to accounts. Every node of a cluster uses
// the same store, so any node handles any step.
type Store interface {
	// SaveLogin stores a pending sign-in (and may delete expired ones).
	SaveLogin(ctx context.Context, l Login) error
	// TakeLogin removes and returns the pending sign-in of stateHash; ErrNotFound when there is none or it expired.
	// A state is taken at most once.
	TakeLogin(ctx context.Context, stateHash string, now time.Time) (*Login, error)
	// SaveTicket stores a sign-in code.
	SaveTicket(ctx context.Context, t Ticket) error
	// GetTicket returns the sign-in code codeHash: ErrNotFound when it never existed, ErrExpired when it expired or
	// was used.
	GetTicket(ctx context.Context, codeHash string, now time.Time) (*Ticket, error)
	// UseTicket marks the sign-in code used; ErrExpired when it was used (or expired) meanwhile.
	UseTicket(ctx context.Context, codeHash string, now time.Time) error
	// Resolve returns the login of id: the account id is pinned to, else the one account whose login equals the
	// email ignoring case, which it pins id to (ErrUnknownAccount, ErrAmbiguous, ErrMismatch otherwise).
	Resolve(ctx context.Context, id Identity) (string, error)
	// Linked reports which of usernames have an identity pinned.
	Linked(ctx context.Context, usernames []string) (map[string]bool, error)
	// Unlink removes the identities pinned to username (a member of the tenant of ctx on Postgres); the next
	// sign-in pins again.
	Unlink(ctx context.Context, username string) error
}

// Method is how a member of an organization may sign in.
type Method string

// Sign-in methods of a membership.
const (
	MethodBoth     Method = "both"
	MethodPassword Method = "password"
	MethodOIDC     Method = "oidc"
)

// ParseMethod validates s.
func ParseMethod(s string) (Method, error) {
	switch m := Method(s); m {
	case MethodBoth, MethodPassword, MethodOIDC:
		return m, nil
	}
	return "", fmt.Errorf("sign_in must be %q, %q or %q, not %q", MethodBoth, MethodPassword, MethodOIDC, s)
}

// Policy is the sign-in policy of one membership.
type Policy struct {
	// System: the membership is in the platform operators' organization, which signs in through OIDC only.
	System bool
	// RequireOIDC: the organization requires OIDC sign-in.
	RequireOIDC bool
	// Method is the membership's own setting ("" is MethodBoth).
	Method Method
}

// PasswordAllowed reports whether the membership may sign in with a password.
func (p Policy) PasswordAllowed() bool {
	return !p.System && !p.RequireOIDC && p.Method != MethodOIDC
}

// OIDCAllowed reports whether the membership may sign in through OIDC: always where OIDC is required, otherwise
// unless the member is set to password only.
func (p Policy) OIDCAllowed() bool {
	return p.System || p.RequireOIDC || p.Method != MethodPassword
}

// Policies stores the sign-in settings (Postgres). Standalone servers have none: every user is MethodBoth.
type Policies interface {
	// Policy returns the settings of username in the tenant of ctx (System is left to the caller).
	Policy(ctx context.Context, username string) (Policy, error)
	// RequireOIDC reports whether the tenant of ctx requires OIDC sign-in.
	RequireOIDC(ctx context.Context) (bool, error)
	// SetRequireOIDC sets it for the tenant of ctx (an organization's administrator).
	SetRequireOIDC(ctx context.Context, require bool) error
	// SetMethod sets the method of username in the tenant of ctx; ErrNoSuchUser when it is not a member.
	SetMethod(ctx context.Context, username string, m Method) error
	// Methods returns the method of every member of the tenant of ctx.
	Methods(ctx context.Context) (map[string]Method, error)
}

// RandomToken returns a random URL-safe token of 32 bytes.
func RandomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("oidc: random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Hash is how states, browser bindings and sign-in codes are stored: hex SHA-256.
func Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
