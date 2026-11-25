package pgserver

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/auditlog"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/membership"
	"github.com/openrport/openrport/server/operator"
	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/share/tenant"
)

// organizationID is what operators may name an organization: the tenants id format without a leading "_"
// (reserved for the server's own tenants).
var organizationID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

const (
	generatedPasswordLength = 24
	generatedPasswordChars  = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	maxOrganizationName     = 200
)

// Operator is the Postgres operator.Service. It works on the application connection: tenants change only through
// the operator functions of migration 1050, inside a transaction of the system tenant, and every change is written
// to the system tenant's audit log in the same transaction (the user of AddAdmin is created first, by the user
// service).
type Operator struct {
	db *sqlx.DB
	// memberships adds administrators: new accounts or memberships of existing ones
	memberships *Memberships
	now         func() time.Time
}

var _ operator.Service = (*Operator)(nil)

// NewOperator returns the operator service on db. Users created by AddAdmin are validated with the password and 2FA
// settings of cfg's [api] section.
func NewOperator(db *sqlx.DB, cfg *chconfig.Config) *Operator {
	return &Operator{
		db:          db,
		memberships: NewMemberships(db, cfg),
		now:         time.Now,
	}
}

func (o *Operator) inSystem(ctx context.Context, fn func(tx *sqlx.Tx) error) error {
	err := postgres.InTenantID(ctx, o.db, operator.SystemTenant, fn)
	if isCode(err, "42501") {
		// an application role granted before migration 1050
		return fmt.Errorf("%w (give the application role the operator privileges: run \"rportd migrate --grant-to ROLE\" as the schema owner)", err)
	}
	return err
}

const organizationColumns = "id, name, status, created_at, max_clients"

// List implements operator.Service.
func (o *Operator) List(ctx context.Context) ([]operator.Organization, error) {
	orgs := []operator.Organization{}
	err := o.inSystem(ctx, func(tx *sqlx.Tx) error {
		if err := tx.SelectContext(ctx, &orgs,
			"SELECT "+organizationColumns+" FROM tenants WHERE left(id, 1) <> '_' ORDER BY created_at, id"); err != nil {
			return err
		}
		for i := range orgs {
			if err := counts(ctx, tx, &orgs[i]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list organizations: %w", err)
	}
	return orgs, nil
}

// Get implements operator.Service.
func (o *Operator) Get(ctx context.Context, id string) (*operator.Organization, error) {
	var org *operator.Organization
	err := o.inSystem(ctx, func(tx *sqlx.Tx) (err error) {
		org, err = getOrganization(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return org, nil
}

// getOrganization reads the organization id with its counts. It leaves the transaction in the tenant id (see counts).
func getOrganization(ctx context.Context, tx *sqlx.Tx, id string) (*operator.Organization, error) {
	if strings.HasPrefix(id, "_") {
		return nil, operator.ErrNotFound
	}
	org := &operator.Organization{}
	err := tx.GetContext(ctx, org, "SELECT "+organizationColumns+" FROM tenants WHERE id = $1", id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, operator.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get organization %q: %w", id, err)
	}
	if err := counts(ctx, tx, org); err != nil {
		return nil, fmt.Errorf("get organization %q: %w", id, err)
	}
	return org, nil
}

// counts fills in the device and user counts of org. Row-level security stays on: it switches the transaction to
// org's tenant, so whatever runs after it in tx runs there, not in the system tenant.
func counts(ctx context.Context, tx *sqlx.Tx, org *operator.Organization) error {
	if _, err := tx.ExecContext(ctx, "SELECT set_config($1, $2, true)", postgres.TenantSetting, org.ID); err != nil {
		return err
	}
	return tx.QueryRowxContext(ctx,
		"SELECT (SELECT count(*) FROM clients WHERE tenant_id = $1), (SELECT count(*) FROM users WHERE tenant_id = $1)",
		org.ID).Scan(&org.DeviceCount, &org.UserCount)
}

func validLimit(limit *int) error {
	if limit != nil && (*limit < 0 || *limit > math.MaxInt32) {
		return fmt.Errorf("%w: device_limit must be between 0 and %d", operator.ErrInvalid, math.MaxInt32)
	}
	return nil
}

// Create implements operator.Service.
func (o *Operator) Create(ctx context.Context, by operator.Actor, in operator.NewOrganization) (*operator.Organization, error) {
	if !organizationID.MatchString(in.ID) {
		return nil, fmt.Errorf("%w: the organization id must be 1 to 63 lower-case letters, digits, '-' or '_', starting with a letter or digit", operator.ErrInvalid)
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = in.ID
	}
	if len(in.Name) > maxOrganizationName {
		return nil, fmt.Errorf("%w: the name must be at most %d characters", operator.ErrInvalid, maxOrganizationName)
	}
	if err := validLimit(in.DeviceLimit); err != nil {
		return nil, err
	}
	err := o.inSystem(ctx, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, "SELECT rport_operator_add_tenant($1, $2, $3)", in.ID, in.Name, in.DeviceLimit); err != nil {
			if isCode(err, "23505") {
				return fmt.Errorf("organization %q %w", in.ID, operator.ErrExists)
			}
			return fmt.Errorf("add organization %q: %w", in.ID, err)
		}
		return o.audit(ctx, tx, by, operator.AuditApplicationOrganization, operator.AuditActionCreate, in.ID, in)
	})
	if err != nil {
		return nil, err
	}
	if err := syncBuiltins(ctx, o.db, tenant.ID(in.ID)); err != nil {
		return nil, err
	}
	return o.Get(ctx, in.ID)
}

// SetStatus implements operator.Service.
func (o *Operator) SetStatus(ctx context.Context, by operator.Actor, id, status string) (*operator.Organization, error) {
	action := operator.AuditActionSuspend
	switch status {
	case operator.StatusSuspended:
	case operator.StatusActive:
		action = operator.AuditActionActivate
	default:
		return nil, fmt.Errorf("%w: status must be %q or %q", operator.ErrInvalid, operator.StatusActive, operator.StatusSuspended)
	}
	return o.change(ctx, id, func(tx *sqlx.Tx) error {
		var found bool
		if err := tx.GetContext(ctx, &found, "SELECT rport_operator_set_tenant_status($1, $2)", id, status); err != nil {
			return err
		}
		if !found {
			return operator.ErrNotFound
		}
		return o.audit(ctx, tx, by, operator.AuditApplicationOrganization, action, id, map[string]string{"status": status})
	})
}

// SetDeviceLimit implements operator.Service.
func (o *Operator) SetDeviceLimit(ctx context.Context, by operator.Actor, id string, limit *int) (*operator.Organization, error) {
	if err := validLimit(limit); err != nil {
		return nil, err
	}
	return o.change(ctx, id, func(tx *sqlx.Tx) error {
		var found bool
		if err := tx.GetContext(ctx, &found, "SELECT rport_operator_set_max_clients($1, $2)", id, limit); err != nil {
			return err
		}
		if !found {
			return operator.ErrNotFound
		}
		return o.audit(ctx, tx, by, operator.AuditApplicationOrganization, operator.AuditActionUpdate, id, map[string]*int{"device_limit": limit})
	})
}

// change runs fn in a system tenant transaction for an existing organization id and returns it afterwards.
func (o *Operator) change(ctx context.Context, id string, fn func(tx *sqlx.Tx) error) (*operator.Organization, error) {
	if strings.HasPrefix(id, "_") {
		return nil, operator.ErrNotFound
	}
	var org *operator.Organization
	err := o.inSystem(ctx, func(tx *sqlx.Tx) (err error) {
		if err := fn(tx); err != nil {
			return err
		}
		org, err = getOrganization(ctx, tx, id)
		return err
	})
	if err != nil {
		if errors.Is(err, operator.ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("organization %q: %w", id, err)
	}
	return org, nil
}

// AddAdmin implements operator.Service.
func (o *Operator) AddAdmin(ctx context.Context, by operator.Actor, id string, in operator.NewAdmin) (*operator.Admin, error) {
	var status string
	err := o.db.GetContext(ctx, &status, "SELECT status FROM tenants WHERE id = $1", id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, operator.ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("organization %q: %w", id, err)
	case status == "reserved", strings.HasPrefix(id, "_") && tenant.ID(id) != operator.SystemTenant:
		return nil, operator.ErrNotFound
	case status != operator.StatusActive:
		return nil, fmt.Errorf("organization %q: %w", id, operator.ErrNotActive)
	}
	inv, err := o.memberships.invite(tenant.WithID(ctx, tenant.ID(id)), tenant.ID(id), membership.Invitation{
		Username:    in.Username,
		Groups:      []string{users.Administrators},
		TwoFASendTo: in.TwoFASendTo,
	})
	switch {
	case errors.Is(err, membership.ErrAlreadyMember):
		return nil, fmt.Errorf("%q is a member of %q already: %w", in.Username, id, operator.ErrExists)
	case errors.Is(err, membership.ErrInvalid):
		return nil, fmt.Errorf("%w: %v", operator.ErrInvalid, err)
	case err != nil:
		return nil, err
	}
	err = o.inSystem(ctx, func(tx *sqlx.Tx) error {
		return o.audit(ctx, tx, by, operator.AuditApplicationOrganizationAdmin, operator.AuditActionCreate, id, in)
	})
	if err != nil {
		return nil, fmt.Errorf("admin %q added to %q, but the audit log entry failed: %w", in.Username, id, err)
	}
	return &operator.Admin{Organization: id, Username: in.Username, NewAccount: inv.NewAccount, Password: inv.Password,
		PasswordExpired: inv.PasswordExpired}, nil
}

// Members implements operator.Service.
func (o *Operator) Members(ctx context.Context, id string) ([]operator.Member, error) {
	members := []operator.Member{}
	err := o.inSystem(ctx, func(tx *sqlx.Tx) error {
		if _, err := getOrganization(ctx, tx, id); err != nil {
			return err
		}
		// getOrganization left tx in the organization's tenant
		var rows []struct {
			Username      string `db:"username"`
			Organizations int    `db:"organizations"`
		}
		if err := tx.SelectContext(ctx, &rows, `SELECT u.username,
				(SELECT count(*) FROM user_logins l WHERE l.username = u.username) AS organizations
			FROM users u WHERE u.tenant_id = $1 ORDER BY u.username COLLATE "C"`, id); err != nil {
			return err
		}
		var groups []struct {
			Username string `db:"username"`
			Group    string `db:"group_name"`
		}
		if err := tx.SelectContext(ctx, &groups,
			`SELECT username, group_name FROM user_groups WHERE tenant_id = $1 ORDER BY group_name COLLATE "C"`, id); err != nil {
			return err
		}
		byName := map[string]int{}
		for _, r := range rows {
			byName[r.Username] = len(members)
			members = append(members, operator.Member{Username: r.Username, Groups: []string{}, Organizations: r.Organizations})
		}
		for _, g := range groups {
			if i, ok := byName[g.Username]; ok {
				members[i].Groups = append(members[i].Groups, g.Group)
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, operator.ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("members of %q: %w", id, err)
	}
	return members, nil
}

// audit writes an operator action to the audit log of the system tenant, in tx (which must be in the system tenant).
func (o *Operator) audit(ctx context.Context, tx *sqlx.Tx, by operator.Actor, application, action, id string, request any) error {
	if _, err := tx.ExecContext(ctx, "SELECT set_config($1, $2, true)", postgres.TenantSetting, string(operator.SystemTenant)); err != nil {
		return err
	}
	e := auditlog.Entry{
		Timestamp:   o.now(),
		Username:    by.Username,
		RemoteIP:    by.RemoteIP,
		Application: application,
		Action:      action,
		ID:          id,
	}
	if request != nil {
		b, err := json.Marshal(request)
		if err != nil {
			return err
		}
		e.Request = string(b)
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO auditlog (tenant_id, timestamp, username, remote_ip, application, action, affected_id, request)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		string(operator.SystemTenant), e.Timestamp, e.Username, e.RemoteIP, e.Application, e.Action, e.ID, e.Request)
	if err != nil {
		return fmt.Errorf("audit log: %w", err)
	}
	return nil
}

func isCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// generatePassword returns a random password for a new administrator, shown once.
func generatePassword(n int) (string, error) {
	b := make([]byte, n)
	maxIdx := big.NewInt(int64(len(generatedPasswordChars)))
	for i := range b {
		n, err := rand.Int(rand.Reader, maxIdx)
		if err != nil {
			return "", fmt.Errorf("generate password: %w", err)
		}
		b[i] = generatedPasswordChars[n.Int64()]
	}
	return string(b), nil
}
