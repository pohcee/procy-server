package users

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/storage/postgres"
)

// JoinAccountSetting is the transaction setting that lets a users row join the existing account of its login
// (migration 1400): without it, a username with an account in another tenant is refused as taken.
const JoinAccountSetting = "rport.join_account"

var (
	// ErrNoAccount: the login has no account to join.
	ErrNoAccount = errors.New("the login has no account")
	// ErrAlreadyMember: the login is a member of the tenant already.
	ErrAlreadyMember = errors.New("the login is a member already")
)

// Organization is a tenant a login is a member of.
type Organization struct {
	ID   string `db:"id"`
	Name string `db:"name"`
}

// Organizations returns the active tenants the login username is a member of, by name. It needs no tenant in ctx.
func (s *Store) Organizations(ctx context.Context, username string) ([]Organization, error) {
	orgs := []Organization{}
	err := s.db.SelectContext(ctx, &orgs, `SELECT t.id, t.name FROM user_logins l JOIN tenants t ON t.id = l.tenant_id
		WHERE l.username = $1 AND t.status = 'active' ORDER BY t.name COLLATE "C", t.id COLLATE "C"`, username)
	if err != nil {
		return nil, fmt.Errorf("users: organizations of %q: %w", username, err)
	}
	return orgs, nil
}

// MembershipCount returns how many tenants (of any status) the login username is a member of.
func (s *Store) MembershipCount(ctx context.Context, username string) (int, error) {
	var n int
	if err := s.db.GetContext(ctx, &n, "SELECT count(*) FROM user_logins WHERE username = $1", username); err != nil {
		return 0, fmt.Errorf("users: memberships of %q: %w", username, err)
	}
	return n, nil
}

// MemberElsewhere reports whether the login username is a member of a tenant other than the one of ctx.
func (s *Store) MemberElsewhere(ctx context.Context, username string) (bool, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return false, err
	}
	var ok bool
	if err := s.db.GetContext(ctx, &ok, "SELECT EXISTS (SELECT 1 FROM user_logins WHERE username = $1 AND tenant_id <> $2)",
		username, string(tid)); err != nil {
		return false, fmt.Errorf("users: memberships of %q: %w", username, err)
	}
	return ok, nil
}

// AccountExists reports whether the login username has an account (is a member of any tenant).
func (s *Store) AccountExists(ctx context.Context, username string) (bool, error) {
	var ok bool
	if err := s.db.GetContext(ctx, &ok, "SELECT EXISTS (SELECT 1 FROM accounts WHERE username = $1)", username); err != nil {
		return false, fmt.Errorf("users: account %q: %w", username, err)
	}
	return ok, nil
}

// Remember makes the tenant of ctx the one the next login of username that names no organization goes to. It
// does nothing when username isn't a member there.
func (s *Store) Remember(ctx context.Context, username string) error {
	err := postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, "SELECT rport_remember_tenant($1)", username)
		return err
	})
	if err != nil {
		return fmt.Errorf("users: remember organization of %q: %w", username, grantHint(err))
	}
	return nil
}

// grantHint names the fix when the application role was granted before migration 1400.
func grantHint(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42501" {
		return fmt.Errorf("%w (give the application role the account privileges: run \"rportd migrate --grant-to ROLE\" as the schema owner)", err)
	}
	return err
}

// AddMember makes the existing account usr.Username a member of the tenant of ctx with usr.Groups and
// usr.TwoFASendTo; its credential stays the account's (usr.Password is ignored). ErrNoAccount: there is no such
// account; ErrAlreadyMember: it is a member there already.
func (s *Store) AddMember(ctx context.Context, usr *users.User) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		var exists, member bool
		if err := tx.QueryRowxContext(ctx, `SELECT EXISTS (SELECT 1 FROM accounts WHERE username = $1),
			EXISTS (SELECT 1 FROM users WHERE tenant_id = $2 AND username = $1)`, usr.Username, string(tid)).Scan(&exists, &member); err != nil {
			return err
		}
		switch {
		case !exists:
			return ErrNoAccount
		case member:
			return ErrAlreadyMember
		}
		if _, err := tx.ExecContext(ctx, "SELECT set_config($1, $2, true)", JoinAccountSetting, usr.Username); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO users (tenant_id, username, two_fa_send_to) VALUES ($1, $2, $3)`,
			string(tid), usr.Username, usr.TwoFASendTo); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "SELECT set_config($1, '', true)", JoinAccountSetting); err != nil {
			return err
		}
		return addMemberships(ctx, tx, tid, usr.Username, usr.Groups)
	})
	if errors.Is(err, ErrNoAccount) || errors.Is(err, ErrAlreadyMember) {
		return err
	}
	if err != nil {
		return wrapUserError("add member", usr.Username, err)
	}
	return nil
}
