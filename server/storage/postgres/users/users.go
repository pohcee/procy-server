// Package users stores API users and groups in Postgres: the users.Provider of the OSS API (the same behavior as
// the OSS [database] user provider, with group details), scoped to the tenant of each call.
//
// A login is one account (table accounts: password hash, password expiry, TOTP secret, 2FA delivery address) and a
// member of one or more tenants (a users row per tenant: its groups). Triggers keep accounts and the global
// membership index user_logins in step with users: Add of a username another tenant uses fails unless it is a
// membership added by AddMember, and the tenants of a login are found without knowing one first.
package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"

	extperm "github.com/pohcee/procy-server/plus/capabilities/extendedpermission"
	apierrors "github.com/pohcee/procy-server/server/api/errors"
	"github.com/pohcee/procy-server/server/api/users"
	"github.com/pohcee/procy-server/share/enums"
	"github.com/pohcee/procy-server/share/tenant"

	"github.com/pohcee/procy-server/server/storage/postgres"
)

// Store is a users.Provider on Postgres.
type Store struct {
	db *sqlx.DB
}

var _ users.Provider = (*Store)(nil)

// New returns a Store on db (migrated).
func New(db *sqlx.DB) *Store { return &Store{db: db} }

// Close does nothing: the database is shared and closed by its owner.
func (s *Store) Close() error { return nil }

// Type reports a database provider.
func (s *Store) Type() enums.ProviderSource { return enums.ProviderSourceDB }

// SupportsGroupPermissions is true: groups and their permissions are stored.
func (s *Store) SupportsGroupPermissions() bool { return true }

// TenantOf returns a tenant the login username is a member of (the first by id), and false if no tenant has it. It
// needs no tenant in ctx: it reads the global user_logins table, which has no row-level security.
func (s *Store) TenantOf(ctx context.Context, username string) (tenant.ID, bool, error) {
	var id string
	err := s.db.GetContext(ctx, &id, "SELECT tenant_id FROM user_logins WHERE username = $1 ORDER BY tenant_id LIMIT 1", username)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("users: tenant of login: %w", err)
	}
	return tenant.ID(id), true, nil
}

// userColumns are read from users u joined with its account a (userFrom): the credential is the account's.
const (
	userColumns = "u.username, a.password, a.password_expired, a.two_fa_send_to, a.totp_secret"
	userFrom    = " FROM users u JOIN accounts a ON a.username = u.username"
)

func (s *Store) GetByUsername(ctx context.Context, username string) (*users.User, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var u *users.User
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		found := &users.User{}
		err := tx.GetContext(ctx, found, "SELECT "+userColumns+userFrom+" WHERE u.tenant_id = $1 AND u.username = $2", string(tid), username)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := tx.SelectContext(ctx, &found.Groups,
			`SELECT group_name FROM user_groups WHERE tenant_id = $1 AND username = $2 ORDER BY group_name COLLATE "C"`,
			string(tid), username); err != nil {
			return err
		}
		u = found
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("users: get %q: %w", username, err)
	}
	return u, nil
}

func (s *Store) GetAll(ctx context.Context) ([]*users.User, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var all []*users.User
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		if err := tx.SelectContext(ctx, &all,
			"SELECT "+userColumns+userFrom+` WHERE u.tenant_id = $1 ORDER BY u.username COLLATE "C"`, string(tid)); err != nil {
			return err
		}
		var memberships []struct {
			Username string `db:"username"`
			Group    string `db:"group_name"`
		}
		if err := tx.SelectContext(ctx, &memberships,
			`SELECT username, group_name FROM user_groups WHERE tenant_id = $1 ORDER BY group_name COLLATE "C"`, string(tid)); err != nil {
			return err
		}
		byName := make(map[string]*users.User, len(all))
		for _, u := range all {
			byName[u.Username] = u
		}
		for _, m := range memberships {
			if u := byName[m.Username]; u != nil {
				u.Groups = append(u.Groups, m.Group)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("users: list: %w", err)
	}
	return all, nil
}

// group is a stored group row.
type group struct {
	Name               string                    `db:"name"`
	Permissions        users.Permissions         `db:"permissions"`
	TunnelsRestricted  *extperm.PermissionParams `db:"tunnels_restricted"`
	CommandsRestricted *extperm.PermissionParams `db:"commands_restricted"`
}

func (g group) toGroup() users.Group {
	return users.Group{
		Name:               g.Name,
		Permissions:        g.Permissions,
		TunnelsRestricted:  g.TunnelsRestricted,
		CommandsRestricted: g.CommandsRestricted,
	}
}

const groupColumns = "name, permissions, tunnels_restricted, commands_restricted"

// ListGroups returns the stored groups and, after them, the groups that exist only through a membership (with no
// permissions), as the OSS database provider does.
func (s *Store) ListGroups(ctx context.Context) ([]users.Group, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out []users.Group
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		var stored []group
		if err := tx.SelectContext(ctx, &stored,
			"SELECT "+groupColumns+` FROM groups WHERE tenant_id = $1 ORDER BY name COLLATE "C"`, string(tid)); err != nil {
			return err
		}
		var memberOnly []string
		if err := tx.SelectContext(ctx, &memberOnly, `SELECT DISTINCT ug.group_name COLLATE "C" AS group_name FROM user_groups ug
			WHERE ug.tenant_id = $1
			  AND NOT EXISTS (SELECT 1 FROM groups g WHERE g.tenant_id = ug.tenant_id AND g.name = ug.group_name)
			ORDER BY 1`, string(tid)); err != nil {
			return err
		}
		out = make([]users.Group, 0, len(stored)+len(memberOnly))
		for _, g := range stored {
			out = append(out, g.toGroup())
		}
		for _, name := range memberOnly {
			out = append(out, users.NewGroup(name, nil, nil))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("users: list groups: %w", err)
	}
	return out, nil
}

// GetGroup returns the stored group, or a group without permissions (Administrators: all permissions) if none is
// stored.
func (s *Store) GetGroup(ctx context.Context, name string) (users.Group, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return users.Group{}, err
	}
	out := users.NewGroup(name, nil, nil)
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		var g group
		err := tx.GetContext(ctx, &g, "SELECT "+groupColumns+" FROM groups WHERE tenant_id = $1 AND name = $2", string(tid), name)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = g.toGroup()
		return nil
	})
	if err != nil {
		return users.Group{}, fmt.Errorf("users: get group %q: %w", name, err)
	}
	return out, nil
}

// UpdateGroup stores g under name, replacing what was stored.
func (s *Store) UpdateGroup(ctx context.Context, name string, g users.Group) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	perms, err := g.Permissions.Value()
	if err != nil {
		return err
	}
	tr, err := restrictionValue(g.TunnelsRestricted)
	if err != nil {
		return err
	}
	cr, err := restrictionValue(g.CommandsRestricted)
	if err != nil {
		return err
	}
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO groups (tenant_id, name, permissions, tunnels_restricted, commands_restricted)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, name) DO UPDATE SET permissions = EXCLUDED.permissions,
				tunnels_restricted = EXCLUDED.tunnels_restricted, commands_restricted = EXCLUDED.commands_restricted`,
			string(tid), name, perms, tr, cr)
		return err
	})
	if err != nil {
		return fmt.Errorf("users: update group %q: %w", name, err)
	}
	return nil
}

// restrictionValue is the stored JSON of p, or NULL.
func restrictionValue(p *extperm.PermissionParams) (any, error) {
	if p == nil {
		return nil, nil
	}
	v, err := p.Value()
	if err != nil || v == nil {
		return nil, err
	}
	if b, ok := v.([]byte); ok {
		return string(b), nil
	}
	return v, nil
}

// DeleteGroup removes the group and every membership of it.
func (s *Store) DeleteGroup(ctx context.Context, name string) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM user_groups WHERE tenant_id = $1 AND group_name = $2", string(tid), name); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM groups WHERE tenant_id = $1 AND name = $2", string(tid), name)
		return err
	})
	if err != nil {
		return fmt.Errorf("users: delete group %q: %w", name, err)
	}
	return nil
}

// Add stores a new user with its groups. A username used by any tenant is refused with a 400 API error.
func (s *Store) Add(ctx context.Context, usr *users.User) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	expired := false
	if usr.PasswordExpired != nil {
		expired = *usr.PasswordExpired
	}
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO users (tenant_id, username, password, password_expired, two_fa_send_to, totp_secret)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			string(tid), usr.Username, usr.Password, expired, usr.TwoFASendTo, usr.TotP); err != nil {
			return err
		}
		return addMemberships(ctx, tx, tid, usr.Username, usr.Groups)
	})
	if err != nil {
		return wrapUserError("add", usr.Username, err)
	}
	return nil
}

func addMemberships(ctx context.Context, tx *sqlx.Tx, tid tenant.ID, username string, groups []string) error {
	for _, g := range groups {
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_groups (tenant_id, username, group_name) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, string(tid), username, g); err != nil {
			return err
		}
	}
	return nil
}

// Update changes the non-empty fields of usr on the user usernameToUpdate: a different usr.Username renames it, and
// non-nil usr.Groups replace its groups. Updating an unknown user does nothing.
func (s *Store) Update(ctx context.Context, usr *users.User, usernameToUpdate string) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	if usernameToUpdate == "" {
		return errors.New("cannot update user with empty username")
	}

	var sets []string
	args := []any{string(tid), usernameToUpdate}
	set := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	// the credential is the account's (rport_set_account; NULL leaves a field as it is)
	var password, totp, sendTo any
	var expired any
	credential := false
	if usr.Password != "" {
		password, credential = usr.Password, true
	}
	if usr.PasswordExpired != nil {
		expired, credential = *usr.PasswordExpired, true
	}
	if usr.TotP != "" {
		totp, credential = usr.TotP, true
	}
	if usr.TwoFASendTo != "" {
		sendTo, credential = usr.TwoFASendTo, true
	}
	newName := usernameToUpdate
	if usr.Username != "" && usr.Username != usernameToUpdate {
		set("username", usr.Username)
		newName = usr.Username
	}

	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		var exists bool
		if err := tx.GetContext(ctx, &exists, "SELECT EXISTS (SELECT 1 FROM users WHERE tenant_id = $1 AND username = $2)",
			string(tid), usernameToUpdate); err != nil {
			return err
		}
		if !exists {
			return nil
		}
		if len(sets) > 0 {
			// a rename cascades to user_groups (foreign key), user_logins and accounts (triggers)
			if _, err := tx.ExecContext(ctx, "UPDATE users SET "+strings.Join(sets, ", ")+" WHERE tenant_id = $1 AND username = $2",
				args...); err != nil {
				return err
			}
		}
		if credential {
			if _, err := tx.ExecContext(ctx, "SELECT rport_set_account($1, $2, $3, $4, $5)", newName, password, expired, totp, sendTo); err != nil {
				return grantHint(err)
			}
		}
		if usr.Groups == nil {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM user_groups WHERE tenant_id = $1 AND username = $2", string(tid), newName); err != nil {
			return err
		}
		return addMemberships(ctx, tx, tid, newName, usr.Groups)
	})
	if err != nil {
		return wrapUserError("update", newName, err)
	}
	return nil
}

// Delete removes the user and its memberships. Deleting an unknown user is not an error.
func (s *Store) Delete(ctx context.Context, usernameToDelete string) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM users WHERE tenant_id = $1 AND username = $2", string(tid), usernameToDelete)
		return err
	})
	if err != nil {
		return fmt.Errorf("users: delete %q: %w", usernameToDelete, err)
	}
	return nil
}

// wrapUserError turns a unique violation of the username (in this tenant: users_pkey; an account of another
// tenant: accounts_pkey) into a 400 API error that names the username, and a rename of a login other tenants share
// into a 400 that says so.
func wrapUserError(op, username string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		(pgErr.ConstraintName == "users_pkey" || pgErr.ConstraintName == "user_logins_pkey" || pgErr.ConstraintName == "accounts_pkey") {
		return apierrors.APIError{
			Message:    fmt.Sprintf("username %q is already taken", username),
			HTTPStatus: http.StatusBadRequest,
		}
	}
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "accounts_shared" {
		return apierrors.APIError{
			Message:    "the login belongs to other organizations too: it can't be renamed here",
			HTTPStatus: http.StatusBadRequest,
		}
	}
	return fmt.Errorf("users: %s %q: %w", op, username, err)
}
