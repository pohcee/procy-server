// Package signup is the Postgres storage of self-service signup (server/signup): the signups and org_deletions
// tables of the system tenant (migration 1550), deleting a tenant through rport_signup_delete_tenant, and the export
// of an organization's configuration.
package signup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/operator"
	"github.com/openrport/openrport/server/signup"
	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/share/tenant"
)

// Store is the Postgres signup.Store, on the application connection.
type Store struct{ DB *sqlx.DB }

var _ signup.Store = Store{}

func (s Store) inSystem(ctx context.Context, fn func(tx *sqlx.Tx) error) error {
	return postgres.InTenantID(ctx, s.DB, operator.SystemTenant, fn)
}

// CountSince implements signup.Store.
func (s Store) CountSince(ctx context.Context, since time.Time, ip, domain, email string) (c signup.Counts, err error) {
	err = s.inSystem(ctx, func(tx *sqlx.Tx) error {
		return tx.QueryRowxContext(ctx, `SELECT
			(SELECT count(*) FROM signups WHERE tenant_id = '_system' AND remote_ip = $2 AND created_at >= $1),
			(SELECT count(*) FROM signups WHERE tenant_id = '_system' AND domain = $3 AND created_at >= $1),
			(SELECT count(*) FROM signups WHERE tenant_id = '_system' AND email = $4 AND created_at >= $1)`,
			since, ip, domain, email).Scan(&c.IP, &c.Domain, &c.Email)
	})
	return c, err
}

// Insert implements signup.Store.
func (s Store) Insert(ctx context.Context, r signup.Signup) error {
	return s.inSystem(ctx, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO signups (id, email, domain, remote_ip, org_name, password_hash, token_hash, status, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			r.ID, r.Email, r.Domain, r.RemoteIP, r.OrgName, r.PasswordHash, r.TokenHash, r.Status, r.CreatedAt, r.ExpiresAt)
		return err
	})
}

const signupColumns = `id, email, domain, remote_ip, org_name, password_hash, token_hash, status, created_at, expires_at,
	verified_at, decided_at, decided_by, reason, organization_id`

// Get implements signup.Store.
func (s Store) Get(ctx context.Context, id string) (*signup.Signup, error) {
	r := &signup.Signup{}
	err := s.inSystem(ctx, func(tx *sqlx.Tx) error {
		return tx.GetContext(ctx, r, "SELECT "+signupColumns+" FROM signups WHERE tenant_id = '_system' AND id = $1", id)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, signup.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// List implements signup.Store.
func (s Store) List(ctx context.Context, status string, limit int) ([]signup.Signup, error) {
	out := []signup.Signup{}
	err := s.inSystem(ctx, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &out, "SELECT "+signupColumns+` FROM signups WHERE tenant_id = '_system' AND ($1 = '' OR status = $1)
			ORDER BY created_at DESC, id LIMIT $2`, status, limit)
	})
	return out, err
}

// ClaimToken implements signup.Store.
func (s Store) ClaimToken(ctx context.Context, tokenHash string, now time.Time) (*signup.Signup, error) {
	r := &signup.Signup{}
	err := s.inSystem(ctx, func(tx *sqlx.Tx) error {
		return tx.GetContext(ctx, r, `UPDATE signups SET token_hash = NULL
			WHERE tenant_id = '_system' AND token_hash = $1 AND status = 'pending' AND expires_at > $2
			RETURNING `+signupColumns, tokenHash, now)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, signup.ErrToken
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// RestoreToken implements signup.Store.
func (s Store) RestoreToken(ctx context.Context, id, tokenHash string) error {
	return s.inSystem(ctx, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE signups SET token_hash = $2 WHERE tenant_id = '_system' AND id = $1 AND status = 'pending' AND token_hash IS NULL`,
			id, tokenHash)
		return err
	})
}

// SetStatus implements signup.Store.
func (s Store) SetStatus(ctx context.Context, id string, u signup.Update) error {
	return s.inSystem(ctx, func(tx *sqlx.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE signups SET status = $2, verified_at = coalesce($3, verified_at),
			expires_at = coalesce($4, expires_at), decided_at = coalesce($5, decided_at),
			decided_by = CASE WHEN $6 = '' THEN decided_by ELSE $6 END, reason = CASE WHEN $7 = '' THEN reason ELSE $7 END,
			organization_id = coalesce(nullif($8, ''), organization_id),
			password_hash = CASE WHEN $9 THEN '' ELSE password_hash END,
			token_hash = CASE WHEN $2 = 'pending' THEN token_hash ELSE NULL END
			WHERE tenant_id = '_system' AND id = $1`,
			id, u.Status, u.VerifiedAt, u.ExpiresAt, u.DecidedAt, u.DecidedBy, u.Reason, u.OrganizationID, u.ClearPassword)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return signup.ErrNotFound
		}
		return nil
	})
}

// Expire implements signup.Store.
func (s Store) Expire(ctx context.Context, now time.Time) (n int, err error) {
	err = s.inSystem(ctx, func(tx *sqlx.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE signups SET status = 'expired', password_hash = '', token_hash = NULL
			WHERE tenant_id = '_system' AND status IN ('pending', 'awaiting_approval') AND expires_at <= $1`, now)
		if err != nil {
			return err
		}
		k, _ := res.RowsAffected()
		n = int(k)
		return nil
	})
	return n, err
}

// UserExists implements signup.Store: logins are unique across tenants (user_logins).
func (s Store) UserExists(ctx context.Context, username string) (bool, error) {
	var ok bool
	err := s.DB.GetContext(ctx, &ok, "SELECT EXISTS (SELECT 1 FROM user_logins WHERE lower(username) = lower($1))", username)
	return ok, err
}

// AdminEmails implements signup.Store: the 2FA address of each administrator, or the login, when it is an email.
func (s Store) AdminEmails(ctx context.Context, org string) ([]string, error) {
	var rows []struct {
		Username string `db:"username"`
		SendTo   string `db:"two_fa_send_to"`
	}
	err := postgres.InTenantID(ctx, s.DB, tenant.ID(org), func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &rows, `SELECT u.username, u.two_fa_send_to FROM users u
			JOIN user_groups g ON g.tenant_id = u.tenant_id AND g.username = u.username
			WHERE u.tenant_id = $1 AND g.group_name = $2 ORDER BY u.username`, org, users.Administrators)
	})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		switch {
		case strings.Contains(r.SendTo, "@"):
			out = append(out, r.SendTo)
		case strings.Contains(r.Username, "@"):
			out = append(out, r.Username)
		}
	}
	return out, nil
}

const deletionColumns = `id, organization_id, organization_name, state, requested_at, requested_by, reason, delete_after,
	cancel_token_hash, previous_status, finished_at, finished_by`

// AddDeletion implements signup.Store.
func (s Store) AddDeletion(ctx context.Context, d signup.Deletion) (*signup.Deletion, error) {
	out := &signup.Deletion{}
	err := s.inSystem(ctx, func(tx *sqlx.Tx) error {
		return tx.GetContext(ctx, out, `INSERT INTO org_deletions (organization_id, organization_name, state, requested_at, requested_by,
			reason, delete_after, cancel_token_hash, previous_status) VALUES ($1, $2, 'pending', $3, $4, $5, $6, $7, $8)
			RETURNING `+deletionColumns,
			d.OrganizationID, d.OrganizationName, d.RequestedAt, d.RequestedBy, d.Reason, d.DeleteAfter, d.CancelTokenHash, d.PreviousStatus)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return nil, fmt.Errorf("%w: the deletion of organization %s is pending already", signup.ErrConflict, d.OrganizationID)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s Store) getDeletion(ctx context.Context, where string, arg any) (*signup.Deletion, error) {
	d := &signup.Deletion{}
	err := s.inSystem(ctx, func(tx *sqlx.Tx) error {
		return tx.GetContext(ctx, d, "SELECT "+deletionColumns+" FROM org_deletions WHERE tenant_id = '_system' AND state = 'pending' AND "+where, arg)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, signup.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// PendingDeletion implements signup.Store.
func (s Store) PendingDeletion(ctx context.Context, org string) (*signup.Deletion, error) {
	return s.getDeletion(ctx, "organization_id = $1", org)
}

// DeletionByCancelToken implements signup.Store.
func (s Store) DeletionByCancelToken(ctx context.Context, tokenHash string) (*signup.Deletion, error) {
	return s.getDeletion(ctx, "cancel_token_hash = $1", tokenHash)
}

// FinishDeletion implements signup.Store.
func (s Store) FinishDeletion(ctx context.Context, org, state, by string, at time.Time) (*signup.Deletion, error) {
	d := &signup.Deletion{}
	err := s.inSystem(ctx, func(tx *sqlx.Tx) error {
		return tx.GetContext(ctx, d, `UPDATE org_deletions SET state = $2, finished_at = $3, finished_by = $4, cancel_token_hash = NULL
			WHERE tenant_id = '_system' AND organization_id = $1 AND state = 'pending' RETURNING `+deletionColumns, org, state, at, by)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, signup.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// SetDeleteAfter implements signup.Store.
func (s Store) SetDeleteAfter(ctx context.Context, org string, at time.Time) error {
	return s.inSystem(ctx, func(tx *sqlx.Tx) error {
		// a zero time is the database's now(): rport_signup_delete_tenant compares with that clock
		var t *time.Time
		if !at.IsZero() {
			t = &at
		}
		res, err := tx.ExecContext(ctx, "UPDATE org_deletions SET delete_after = coalesce($2, now()) WHERE tenant_id = '_system' AND organization_id = $1 AND state = 'pending'", org, t)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return signup.ErrNotFound
		}
		return nil
	})
}

// DueDeletions implements signup.Store. Due is by the database's clock (the one rport_signup_delete_tenant
// checks), so now is not used.
func (s Store) DueDeletions(ctx context.Context, _ time.Time) ([]signup.Deletion, error) {
	var out []signup.Deletion
	err := s.inSystem(ctx, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &out, "SELECT "+deletionColumns+` FROM org_deletions
			WHERE tenant_id = '_system' AND state = 'pending' AND delete_after <= now() ORDER BY delete_after`)
	})
	return out, err
}

// ListDeletions implements signup.Store.
func (s Store) ListDeletions(ctx context.Context, state string, limit int) ([]signup.Deletion, error) {
	out := []signup.Deletion{}
	err := s.inSystem(ctx, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &out, "SELECT "+deletionColumns+` FROM org_deletions
			WHERE tenant_id = '_system' AND ($1 = '' OR state = $1) ORDER BY requested_at DESC, id DESC LIMIT $2`, state, limit)
	})
	return out, err
}

// DeleteOrganization implements signup.Store. The deletion's date is compared with the database's clock (the
// function's now()), so at is only recorded.
func (s Store) DeleteOrganization(ctx context.Context, org, by string, at time.Time) (deleted bool, err error) {
	err = s.inSystem(ctx, func(tx *sqlx.Tx) error {
		if err := tx.GetContext(ctx, &deleted, "SELECT rport_signup_delete_tenant($1)", org); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "42501" {
				return fmt.Errorf("%w (give the application role the signup privileges: run \"rportd migrate --grant-to ROLE\" as the schema owner)", err)
			}
			return err
		}
		if !deleted {
			return nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE org_deletions SET state = 'done', finished_at = $2, finished_by = $3, cancel_token_hash = NULL
			WHERE tenant_id = '_system' AND organization_id = $1 AND state = 'pending'`, org, at, by)
		return err
	})
	return deleted, err
}

// Audit implements signup.Store: an entry of the system tenant's audit log.
func (s Store) Audit(ctx context.Context, e signup.AuditEntry) error {
	var request string
	if e.Request != nil {
		b, err := json.Marshal(e.Request)
		if err != nil {
			return err
		}
		request = string(b)
	}
	return s.inSystem(ctx, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO auditlog (tenant_id, timestamp, username, remote_ip, application, action, affected_id, request)
			 VALUES ('_system', $1, $2, $3, $4, $5, $6, $7)`,
			e.Time, e.Actor.Username, e.Actor.RemoteIP, e.Application, e.Action, e.ID, request)
		if err != nil {
			return fmt.Errorf("audit log: %w", err)
		}
		return nil
	})
}
