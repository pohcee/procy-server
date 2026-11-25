// Package billingprovider is the Postgres billing.Store (migration 1500): the payment provider's accounts, webhook
// events, usage reports and dunning notices. It works on the application connection; every write goes through
// the migration's SECURITY DEFINER functions, in a transaction of the context's tenant: the system tenant for the
// webhook, the jobs and operators (every organization), an organization's own for its checkout (its customer
// only).
package billingprovider

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/billing"
	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/share/tenant"
)

// Store is the Postgres billing.Store.
type Store struct {
	DB *sqlx.DB
}

var _ billing.Store = Store{}

const accountColumns = `tenant_id, provider, customer_id, billing_email, subscription_id, subscription_status,
	subscription_event_at, to_jsonb(price_ids)::text AS price_ids_json, has_payment_method, dunning_state,
	payment_failed_at, failed_invoice, read_only_at, suspension_proposed_at, updated_at, updated_by`

type accountRow struct {
	billing.ProviderAccount
	PriceIDsJSON string `db:"price_ids_json"`
}

func (r accountRow) account() (billing.ProviderAccount, error) {
	a := r.ProviderAccount
	if err := json.Unmarshal([]byte(r.PriceIDsJSON), &a.PriceIDs); err != nil {
		return a, err
	}
	return a, nil
}

// Account implements billing.Store.
func (s Store) Account(ctx context.Context, org string) (*billing.ProviderAccount, error) {
	var out *billing.ProviderAccount
	err := postgres.InTenantID(ctx, s.DB, tenant.ID(org), func(tx *sqlx.Tx) (err error) {
		out, err = accountIn(ctx, tx, org)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("billing account: %w", err)
	}
	return out, nil
}

func accountIn(ctx context.Context, tx *sqlx.Tx, org string) (*billing.ProviderAccount, error) {
	var r accountRow
	err := tx.GetContext(ctx, &r, "SELECT "+accountColumns+" FROM billing_provider_accounts WHERE tenant_id = $1", org)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a, err := r.account()
	return &a, err
}

// inCtx runs fn in a transaction of the context's tenant.
func (s Store) inCtx(ctx context.Context, fn func(tx *sqlx.Tx) error) error {
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		return err
	}
	return postgres.InTenantID(ctx, s.DB, tid, fn)
}

// lockAccount serializes the changes of org's account until the transaction ends (webhooks, the dunning tick,
// operators and checkout each read, change and write the whole account).
func lockAccount(ctx context.Context, tx *sqlx.Tx, org string) error {
	_, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext('rport_billing_provider_accounts'), hashtext($1))", org)
	return err
}

// lockedAccount locks org's account and reads it (nil without one).
func lockedAccount(ctx context.Context, tx *sqlx.Tx, org string) (*billing.ProviderAccount, error) {
	if err := lockAccount(ctx, tx, org); err != nil {
		return nil, err
	}
	var rows []accountRow
	if err := tx.SelectContext(ctx, &rows, "SELECT "+accountColumns+" FROM rport_billing_account($1)", org); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	a, err := rows[0].account()
	return &a, err
}

// UpdateAccount implements billing.Store.
func (s Store) UpdateAccount(ctx context.Context, org string, update func(*billing.ProviderAccount) (*billing.ProviderAccount, error)) error {
	err := s.inCtx(ctx, func(tx *sqlx.Tx) error {
		cur, err := lockedAccount(ctx, tx, org)
		if err != nil {
			return err
		}
		next, err := update(cur)
		if err != nil || next == nil {
			return err
		}
		return putAccount(ctx, tx, next)
	})
	if err != nil {
		return fmt.Errorf("billing account %q: %w", org, err)
	}
	return nil
}

// Accounts implements billing.Store (system tenant).
func (s Store) Accounts(ctx context.Context) ([]billing.ProviderAccount, error) {
	var rows []accountRow
	err := s.inCtx(ctx, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &rows, "SELECT "+accountColumns+" FROM rport_billing_accounts()")
	})
	if err != nil {
		return nil, fmt.Errorf("billing accounts: %w", err)
	}
	out := make([]billing.ProviderAccount, 0, len(rows))
	for _, r := range rows {
		a, err := r.account()
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// OrganizationOf implements billing.Store.
func (s Store) OrganizationOf(ctx context.Context, provider, customerID string) (string, error) {
	var org sql.NullString
	err := s.inCtx(ctx, func(tx *sqlx.Tx) error {
		return tx.GetContext(ctx, &org, "SELECT rport_billing_customer_tenant($1, $2)", provider, customerID)
	})
	if err != nil {
		return "", fmt.Errorf("billing customer: %w", err)
	}
	return org.String, nil
}

type accountJSON struct {
	Provider            string     `json:"provider"`
	CustomerID          string     `json:"customer_id"`
	BillingEmail        string     `json:"billing_email"`
	SubscriptionID      string     `json:"subscription_id"`
	SubscriptionStatus  string     `json:"subscription_status"`
	SubscriptionEventAt *time.Time `json:"subscription_event_at"`
	PriceIDs            []string   `json:"price_ids"`
	HasPaymentMethod    bool       `json:"has_payment_method"`
	DunningState        string     `json:"dunning_state"`
	PaymentFailedAt     *time.Time `json:"payment_failed_at"`
	FailedInvoice       string     `json:"failed_invoice"`
	ReadOnlyAt          *time.Time `json:"read_only_at"`
	ProposedAt          *time.Time `json:"suspension_proposed_at"`
	UpdatedBy           string     `json:"updated_by"`
}

func putAccount(ctx context.Context, ex sqlx.ExecerContext, a *billing.ProviderAccount) error {
	state := a.State
	if state == "" {
		state = billing.DunningNone
	}
	ids := a.PriceIDs
	if ids == nil {
		ids = []string{}
	}
	b, err := json.Marshal(accountJSON{
		Provider: a.Provider, CustomerID: a.CustomerID, BillingEmail: a.BillingEmail, SubscriptionID: a.SubscriptionID,
		SubscriptionStatus: string(a.SubscriptionStatus), SubscriptionEventAt: a.SubscriptionEventAt, PriceIDs: ids,
		HasPaymentMethod: a.HasPaymentMethod, DunningState: string(state), PaymentFailedAt: a.FailedAt,
		FailedInvoice: a.FailedInvoice, ReadOnlyAt: a.ReadOnlyAt, ProposedAt: a.ProposedAt, UpdatedBy: a.UpdatedBy,
	})
	if err != nil {
		return err
	}
	_, err = ex.ExecContext(ctx, "SELECT rport_billing_put_account($1, $2)", a.Organization, string(b))
	return err
}

// PutAccount implements billing.Store.
func (s Store) PutAccount(ctx context.Context, a *billing.ProviderAccount) error {
	if err := s.inCtx(ctx, func(tx *sqlx.Tx) error { return putAccount(ctx, tx, a) }); err != nil {
		return fmt.Errorf("billing account %q: %w", a.Organization, err)
	}
	return nil
}

// ApplyEvent implements billing.Store.
func (s Store) ApplyEvent(ctx context.Context, provider, eventID, eventType, org string, update func(*billing.ProviderAccount) (*billing.ProviderAccount, error)) (applied bool, err error) {
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		return false, err
	}
	tx, err := s.DB.BeginTxx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("billing event: %w", err)
	}
	defer func() {
		if err != nil || !applied {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, "SELECT set_config($1, $2, true)", postgres.TenantSetting, string(tid)); err != nil {
		return false, err
	}
	var orgArg any
	if org != "" {
		orgArg = org
	}
	if err = tx.GetContext(ctx, &applied, "SELECT rport_billing_claim_event($1, $2, $3, $4)", provider, eventID, eventType, orgArg); err != nil {
		return false, fmt.Errorf("billing event %s: %w", eventID, err)
	}
	if !applied {
		return false, nil
	}
	var cur *billing.ProviderAccount
	if org != "" {
		if cur, err = lockedAccount(ctx, tx, org); err != nil {
			return false, fmt.Errorf("billing event %s: %w", eventID, err)
		}
	}
	next, err := update(cur)
	if err != nil {
		return false, err
	}
	if next != nil {
		if err = putAccount(ctx, tx, next); err != nil {
			return false, fmt.Errorf("billing event %s: %w", eventID, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return false, fmt.Errorf("billing event %s: %w", eventID, err)
	}
	return true, nil
}

const reportColumns = `tenant_id, period_start, period_end, metric, identifier, customer_id, price_id, quantity, state,
	attempts, first_attempt_at, last_attempt_at, recorded_at, sent_at, confirmed_at, provider_value, last_error`

// Reports implements billing.Store.
func (s Store) Reports(ctx context.Context, periodStart time.Time) ([]billing.UsageReport, error) {
	var arg any
	if !periodStart.IsZero() {
		arg = periodStart
	}
	out := []billing.UsageReport{}
	if err := s.inCtx(ctx, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &out, "SELECT "+reportColumns+" FROM rport_billing_reports($1)", arg)
	}); err != nil {
		return nil, fmt.Errorf("billing usage reports: %w", err)
	}
	for i := range out {
		out[i].PeriodStart = out[i].PeriodStart.UTC()
		out[i].PeriodEnd = out[i].PeriodEnd.UTC()
	}
	return out, nil
}

// PutReport implements billing.Store.
func (s Store) PutReport(ctx context.Context, r *billing.UsageReport) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := s.inCtx(ctx, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, "SELECT rport_billing_put_report($1, $2)", r.Organization, string(b))
		return err
	}); err != nil {
		return fmt.Errorf("billing usage report %s: %w", r.Identifier, err)
	}
	return nil
}

// ClaimNotice implements billing.Store.
func (s Store) ClaimNotice(ctx context.Context, org, kind, episode string) (bool, error) {
	var ok bool
	if err := s.inCtx(ctx, func(tx *sqlx.Tx) error {
		return tx.GetContext(ctx, &ok, "SELECT rport_billing_claim_dunning_notice($1, $2, $3)", org, kind, episode)
	}); err != nil {
		return false, fmt.Errorf("billing notice: %w", err)
	}
	return ok, nil
}

// PruneEvents removes webhook event ids older than keep (system tenant).
func (s Store) PruneEvents(ctx context.Context, keep time.Duration) error {
	return s.inCtx(ctx, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, "SELECT rport_billing_prune_events(make_interval(secs => $1))", keep.Seconds())
		return err
	})
}

// systemTenant is the platform operators' tenant (operator.SystemTenant).
const systemTenant = "_system"

// Audit implements billing.Store.
func (s Store) Audit(ctx context.Context, by billing.Actor, action, org string, request any) error {
	var req string
	if request != nil {
		b, err := json.Marshal(request)
		if err != nil {
			return err
		}
		req = string(b)
	}
	now := time.Now().UTC()
	for _, tid := range []string{systemTenant, org} {
		err := postgres.InTenantID(ctx, s.DB, tenant.ID(tid), func(tx *sqlx.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO auditlog (tenant_id, timestamp, username, remote_ip, application, action, affected_id, request)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, tid, now, by.Username, by.RemoteIP, billing.AuditApplication, action, org, req)
			return err
		})
		if err != nil {
			return fmt.Errorf("billing audit: %w", err)
		}
	}
	return nil
}

// AdminEmails implements billing.Store.
func (s Store) AdminEmails(ctx context.Context, org string) ([]string, error) {
	var rows []struct {
		Username string `db:"username"`
		SendTo   string `db:"two_fa_send_to"`
	}
	err := postgres.InTenantID(ctx, s.DB, tenant.ID(org), func(tx *sqlx.Tx) error {
		// the 2FA address belongs to the login's account (migration 1400), shared by its organizations
		return tx.SelectContext(ctx, &rows, `SELECT u.username, coalesce(nullif(a.two_fa_send_to, ''), u.two_fa_send_to) AS two_fa_send_to
			FROM users u
			JOIN user_groups g ON g.tenant_id = u.tenant_id AND g.username = u.username
			LEFT JOIN accounts a ON a.username = u.username
			WHERE u.tenant_id = $1 AND g.group_name = $2 ORDER BY u.username`, org, users.Administrators)
	})
	if err != nil {
		return nil, fmt.Errorf("billing recipients: %w", err)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, r := range rows {
		for _, v := range []string{r.SendTo, r.Username} {
			if isEmail(v) && !seen[strings.ToLower(v)] {
				seen[strings.ToLower(v)] = true
				out = append(out, v)
				break
			}
		}
	}
	return out, nil
}

func isEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	return at > 0 && at < len(s)-1 && !strings.ContainsAny(s, " \t\r\n<>,;") && strings.Contains(s[at:], ".")
}
