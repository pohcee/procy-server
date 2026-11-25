// Package billing is the Postgres storage of billing plans, statuses and limits (migration 1450): Store serves an
// organization's administrators and the processes' transfer policies, Operator the platform operators, and Upkeep
// keeps the records (trial and comped ends, the transfer policy every few minutes, the period close).
package billing

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

	"github.com/openrport/openrport/server/billing"
	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/share/tenant"
)

// systemTenant is the platform operators' tenant (operator.SystemTenant; not imported to keep this package free of
// the API).
const systemTenant tenant.ID = "_system"

const accountColumns = `tenant_id, plan, status, trial_ends_at, comped_until, reason, exempt_limits, spending_limit_cents,
	has_payment_method, provider_customer_id, provider_subscription_id, cap_lifted_until, extra_transfer_bytes,
	extra_transfer_period, started_at, updated_at, updated_by, transfer_state, transfer_state_at, allowance_bytes,
	used_bytes, soft_cap_bytes, hard_cap_bytes, overage_cents, tunnel_rate, throttle_rate, plan_max_clients, fail_open`

const periodColumns = `tenant_id, period_start, period_end, billed_from, proration, plan, billing_status,
	billable_devices, peak_devices, device_days, extra_devices, tunnel_bytes_to_user, tunnel_bytes_to_device,
	upload_bytes_to_devices, transfer_bytes, allowance_bytes, overage_bytes, overage_gib, base_cents, device_cents,
	overage_cents, total_cents, state, closed_at, approved_by, approved_at, reported_at`

// row is an org_billing row.
type row struct {
	billing.Billing
	AllowanceBytes int64         `db:"allowance_bytes"`
	UsedBytes      int64         `db:"used_bytes"`
	SoftCapBytes   int64         `db:"soft_cap_bytes"`
	HardCapBytes   int64         `db:"hard_cap_bytes"`
	OverageCents   int64         `db:"overage_cents"`
	TunnelRate     int64         `db:"tunnel_rate"`
	ThrottleRate   int64         `db:"throttle_rate"`
	PlanMaxClients sql.NullInt64 `db:"plan_max_clients"`
	FailOpen       string        `db:"fail_open"`
}

func (r *row) account(plans *billing.Plans) *billing.Account {
	b := r.Billing
	a := &billing.Account{Billing: b, PlanDetails: billing.EffectivePlan(&b, plans)}
	a.Policy = billing.Policy{
		Organization: b.Organization, Plan: a.PlanDetails.ID, State: b.TransferState,
		AllowanceBytes: r.AllowanceBytes, UsedBytes: r.UsedBytes, SoftCapBytes: r.SoftCapBytes, HardCapBytes: r.HardCapBytes,
		OverageCents: r.OverageCents, TunnelRate: billing.RateLimit(r.TunnelRate), ThrottleRate: billing.RateLimit(r.ThrottleRate),
		MaxClients: billing.MaxClients(&b, plans), FailOpen: r.FailOpen,
	}
	a.SoftCapReached = a.Policy.SoftCapReached()
	return a
}

// Store serves an organization's billing (on the application connection, in the organization's tenant) and the
// transfer policies.
type Store struct {
	DB    *sqlx.DB
	Plans *billing.Plans
}

var (
	_ billing.OrgService   = Store{}
	_ billing.PolicySource = Store{}
)

// Account implements billing.OrgService: the organization's record, or the default (free plan, paying) before the
// upkeep created it.
func (s Store) Account(ctx context.Context, tid tenant.ID) (*billing.Account, error) {
	var rows []row
	err := postgres.InTenantID(ctx, s.DB, tid, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &rows, "SELECT "+accountColumns+" FROM org_billing WHERE tenant_id = $1", string(tid))
	})
	if err != nil {
		return nil, fmt.Errorf("billing: %w", err)
	}
	if len(rows) == 0 {
		return billing.DefaultAccount(string(tid), s.Plans), nil
	}
	return rows[0].account(s.Plans), nil
}

// Periods implements billing.OrgService: the last 36 closed periods, newest first.
func (s Store) Periods(ctx context.Context, tid tenant.ID) ([]billing.Period, error) {
	out := []billing.Period{}
	err := postgres.InTenantID(ctx, s.DB, tid, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &out, "SELECT "+periodColumns+" FROM billing_periods WHERE tenant_id = $1 ORDER BY period_start DESC LIMIT 36", string(tid))
	})
	if err != nil {
		return nil, fmt.Errorf("billing: %w", err)
	}
	return out, nil
}

// SetSpendingLimit implements billing.OrgService. Only plans that bill overage have a spending limit; the change is
// written to the organization's audit log.
func (s Store) SetSpendingLimit(ctx context.Context, tid tenant.ID, by billing.Actor, cents *int64) (*billing.Account, error) {
	a, err := s.Account(ctx, tid)
	if err != nil {
		return nil, err
	}
	if a.PlanDetails.Overage != billing.OverageBill {
		return nil, fmt.Errorf("%w: the plan %q has no overage, so it has no spending limit", billing.ErrInvalid, a.PlanDetails.Name)
	}
	if cents != nil && (*cents < 0 || *cents > 100_000_000_000) {
		return nil, fmt.Errorf("%w: the spending limit must be between 0 and 1000000000", billing.ErrInvalid)
	}
	err = postgres.InTenantID(ctx, s.DB, tid, func(tx *sqlx.Tx) error {
		var found bool
		if err := tx.GetContext(ctx, &found, "SELECT rport_billing_set_spending_limit($1, $2)", cents, by.Username); err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: the organization has no billing record yet; try again in a few minutes", billing.ErrNotFound)
		}
		return audit(ctx, tx, tid, by, billing.AuditActionSpending, string(tid), map[string]*int64{"spending_limit_cents": cents})
	})
	if err != nil {
		return nil, fmt.Errorf("billing: %w", err)
	}
	return s.Account(ctx, tid)
}

// TransferPolicies implements billing.PolicySource (device_transfer_policy()).
func (s Store) TransferPolicies(ctx context.Context) ([]billing.TenantPolicy, error) {
	return TransferPolicies(ctx, s.DB)
}

// TransferPolicies reads device_transfer_policy() on db (the application or the device role).
func TransferPolicies(ctx context.Context, db *sqlx.DB) ([]billing.TenantPolicy, error) {
	var out []billing.TenantPolicy
	if err := db.SelectContext(ctx, &out, "SELECT tenant_id, plan, transfer_state, tunnel_rate, throttle_rate, updated_at FROM device_transfer_policy()"); err != nil {
		return nil, fmt.Errorf("billing: transfer policies: %w", err)
	}
	return out, nil
}

// PolicySource is the billing.PolicySource of a device process (the device role).
type PolicySource struct{ DB *sqlx.DB }

// TransferPolicies implements billing.PolicySource.
func (p PolicySource) TransferPolicies(ctx context.Context) ([]billing.TenantPolicy, error) {
	return TransferPolicies(ctx, p.DB)
}

// audit writes a billing change to tid's audit log, in tx; it leaves tx in tid.
func audit(ctx context.Context, tx *sqlx.Tx, tid tenant.ID, by billing.Actor, action, id string, request any) error {
	if _, err := tx.ExecContext(ctx, "SELECT set_config($1, $2, true)", postgres.TenantSetting, string(tid)); err != nil {
		return err
	}
	var req *string
	if request != nil {
		b, err := json.Marshal(request)
		if err != nil {
			return err
		}
		s := string(b)
		req = &s
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO auditlog (tenant_id, timestamp, username, remote_ip, application, action, affected_id, request)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		string(tid), time.Now().UTC(), by.Username, by.RemoteIP, billing.AuditApplication, action, id, req)
	if err != nil {
		return fmt.Errorf("audit log: %w", err)
	}
	return nil
}

// auditBoth writes a billing change of organization id to the system tenant's audit log and to the organization's;
// it leaves tx in the system tenant.
func auditBoth(ctx context.Context, tx *sqlx.Tx, by billing.Actor, action, id string, request any) error {
	if err := audit(ctx, tx, tenant.ID(id), by, action, id, request); err != nil {
		return err
	}
	return audit(ctx, tx, systemTenant, by, action, id, request)
}

func isCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

func inSystem(ctx context.Context, db *sqlx.DB, fn func(tx *sqlx.Tx) error) error {
	err := postgres.InTenantID(ctx, db, systemTenant, fn)
	if isCode(err, "42501") || isCode(err, "42883") {
		return fmt.Errorf("%w (run \"rportd migrate\" and \"rportd migrate --grant-to ROLE\" as the schema owner)", err)
	}
	return err
}

func validOrganization(id string) bool { return id != "" && !strings.HasPrefix(id, "_") }
