package users

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/server/storage/postgres"
)

// JoinAccountSetting is the transaction setting that lets a users row join the existing account of its login
// (migration 1400): without it, a username with an account in another tenant is refused as taken.
const JoinAccountSetting = "rport.join_account"

var (
	// ErrNoAccount: the login has no account.
	ErrNoAccount = errors.New("the login has no account")
	// ErrAlreadyMember: the login is a member of the tenant already.
	ErrAlreadyMember = errors.New("the login is a member already")
	// ErrNoInvitation: the tenant has no pending invitation of the login.
	ErrNoInvitation = errors.New("no pending invitation")
)

// Organization is a tenant a login is a member of.
type Organization struct {
	ID   string `db:"id"`
	Name string `db:"name"`
}

// Invitation is a pending invitation of an existing account into a tenant.
type Invitation struct {
	TenantID  string    `db:"id"`
	Name      string    `db:"name"`
	Username  string    `db:"username"`
	Groups    []string  `db:"-"`
	InvitedBy string    `db:"invited_by"`
	CreatedAt time.Time `db:"created_at"`
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
	if err := s.db.GetContext(ctx, &ok, "SELECT rport_account_exists($1)", username); err != nil {
		return false, fmt.Errorf("users: account %q: %w", username, grantHint(err))
	}
	return ok, nil
}

// Invite records the tenant of ctx's invitation of the existing account username with groups, by invitedBy,
// replacing an earlier one. ErrNoAccount: there is no such account; ErrAlreadyMember: it is a member there.
func (s *Store) Invite(ctx context.Context, username string, groups []string, invitedBy string) error {
	if groups == nil {
		groups = []string{}
	}
	g, err := json.Marshal(groups)
	if err != nil {
		return err
	}
	var result string
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		return tx.GetContext(ctx, &result, "SELECT rport_invite($1, $2, $3)", username, string(g), invitedBy)
	})
	if err != nil {
		return fmt.Errorf("users: invite %q: %w", username, grantHint(err))
	}
	switch result {
	case "invited":
		return nil
	case "member":
		return ErrAlreadyMember
	default:
		return ErrNoAccount
	}
}

// Invitations returns the pending invitations of the login username from active tenants, by name. It needs no
// tenant in ctx.
func (s *Store) Invitations(ctx context.Context, username string) ([]Invitation, error) {
	out := []Invitation{}
	if err := s.db.SelectContext(ctx, &out, "SELECT id, name, invited_by, created_at FROM rport_invitations_of($1)", username); err != nil {
		return nil, fmt.Errorf("users: invitations of %q: %w", username, grantHint(err))
	}
	for i := range out {
		out[i].Username = username
	}
	return out, nil
}

// PendingInvitations returns the tenant of ctx's pending invitations, by login.
func (s *Store) PendingInvitations(ctx context.Context) ([]Invitation, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	out := []Invitation{}
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		var rows []struct {
			Username  string    `db:"username"`
			Groups    string    `db:"groups"`
			InvitedBy string    `db:"invited_by"`
			CreatedAt time.Time `db:"created_at"`
		}
		if err := tx.SelectContext(ctx, &rows, `SELECT username, groups, invited_by, created_at FROM membership_invitations
			WHERE tenant_id = $1 ORDER BY username COLLATE "C"`, string(tid)); err != nil {
			return err
		}
		for _, r := range rows {
			inv := Invitation{TenantID: string(tid), Username: r.Username, InvitedBy: r.InvitedBy, CreatedAt: r.CreatedAt}
			if err := json.Unmarshal([]byte(r.Groups), &inv.Groups); err != nil {
				return err
			}
			out = append(out, inv)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("users: pending invitations: %w", err)
	}
	return out, nil
}

// DeleteInvitation removes the tenant of ctx's pending invitation of username (declined or withdrawn).
// ErrNoInvitation when there is none.
func (s *Store) DeleteInvitation(ctx context.Context, username string) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	var n int64
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM membership_invitations WHERE tenant_id = $1 AND username = $2", string(tid), username)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return fmt.Errorf("users: delete invitation of %q: %w", username, err)
	}
	if n == 0 {
		return ErrNoInvitation
	}
	return nil
}

// AcceptInvitation makes the account username a member of the tenant of ctx with the groups of its pending
// invitation there, and removes the invitation. Only the account's owner may do it (the caller checks).
// ErrNoInvitation: there is none; ErrAlreadyMember: it is a member there already.
func (s *Store) AcceptInvitation(ctx context.Context, username string) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		var groups string
		err := tx.GetContext(ctx, &groups, `SELECT groups FROM membership_invitations WHERE tenant_id = $1 AND username = $2
			FOR UPDATE`, string(tid), username)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoInvitation
		}
		if err != nil {
			return err
		}
		var gs []string
		if err := json.Unmarshal([]byte(groups), &gs); err != nil {
			return err
		}
		if err := join(ctx, tx, string(tid), username, gs); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM membership_invitations WHERE tenant_id = $1 AND username = $2", string(tid), username)
		return err
	})
	if errors.Is(err, ErrNoInvitation) || errors.Is(err, ErrAlreadyMember) || errors.Is(err, ErrNoAccount) {
		return err
	}
	if err != nil {
		return wrapUserError("accept invitation", username, err)
	}
	return nil
}

// join inserts the membership of the existing account username into tid (in tx, a transaction of tid).
func join(ctx context.Context, tx *sqlx.Tx, tid, username string, groups []string) error {
	var member bool
	if err := tx.GetContext(ctx, &member, "SELECT EXISTS (SELECT 1 FROM users WHERE tenant_id = $1 AND username = $2)", tid, username); err != nil {
		return err
	}
	if member {
		return ErrAlreadyMember
	}
	if _, err := tx.ExecContext(ctx, "SELECT set_config($1, $2, true)", JoinAccountSetting, username); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO users (tenant_id, username) VALUES ($1, $2)`, tid, username); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "P0002" {
			return ErrNoAccount
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, "SELECT set_config($1, '', true)", JoinAccountSetting); err != nil {
		return err
	}
	for _, g := range groups {
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_groups (tenant_id, username, group_name) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, tid, username, g); err != nil {
			return err
		}
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
