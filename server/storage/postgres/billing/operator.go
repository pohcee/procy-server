package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/billing"
)

// Operator is the billing.OperatorService on the application connection: it works in a transaction of the system
// tenant through the operator functions of migration 1450, and writes every change to the system tenant's audit
// log and the organization's, in the same transaction.
type Operator struct {
	DB    *sqlx.DB
	Plans *billing.Plans
	// Now is time.Now unless set (tests).
	Now func() time.Time
}

var _ billing.OperatorService = (*Operator)(nil)

func (o *Operator) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// accounts reads the accounts of the organizations (one when id is set), in tx (system tenant).
func (o *Operator) accounts(ctx context.Context, tx *sqlx.Tx, id string) ([]billing.Account, error) {
	var ids []string
	q := "SELECT id FROM tenants WHERE left(id, 1) <> '_' AND status IN ('active', 'suspended') AND ($1 = '' OR id = $1) ORDER BY created_at, id"
	if err := tx.SelectContext(ctx, &ids, q, id); err != nil {
		return nil, err
	}
	var rows []row
	var arg any
	if id != "" {
		arg = id
	}
	if err := tx.SelectContext(ctx, &rows, "SELECT "+accountColumns+" FROM rport_operator_billing($1)", arg); err != nil {
		return nil, err
	}
	by := map[string]*row{}
	for i := range rows {
		by[rows[i].Organization] = &rows[i]
	}
	out := make([]billing.Account, 0, len(ids))
	for _, oid := range ids {
		if r := by[oid]; r != nil {
			out = append(out, *r.account(o.Plans))
		} else {
			out = append(out, *billing.DefaultAccount(oid, o.Plans))
		}
	}
	return out, nil
}

// Accounts implements billing.OperatorService.
func (o *Operator) Accounts(ctx context.Context) ([]billing.Account, error) {
	var out []billing.Account
	err := inSystem(ctx, o.DB, func(tx *sqlx.Tx) (err error) {
		out, err = o.accounts(ctx, tx, "")
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("billing: %w", err)
	}
	return out, nil
}

// Account implements billing.OperatorService.
func (o *Operator) Account(ctx context.Context, id string) (*billing.Account, error) {
	if !validOrganization(id) {
		return nil, billing.ErrNotFound
	}
	var out []billing.Account
	err := inSystem(ctx, o.DB, func(tx *sqlx.Tx) (err error) {
		out, err = o.accounts(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("billing: %w", err)
	}
	if len(out) == 0 {
		return nil, billing.ErrNotFound
	}
	return &out[0], nil
}

// change runs fn for an existing organization id in a system tenant transaction, then returns its account.
func (o *Operator) change(ctx context.Context, id string, fn func(tx *sqlx.Tx) (bool, error)) (*billing.Account, error) {
	if !validOrganization(id) {
		return nil, billing.ErrNotFound
	}
	err := inSystem(ctx, o.DB, func(tx *sqlx.Tx) error {
		found, err := fn(tx)
		if err != nil {
			return err
		}
		if !found {
			return billing.ErrNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return o.Account(ctx, id)
}

// SetPlan implements billing.OperatorService. The device limit (tenants.max_clients) becomes the plan's, or the
// trial's for a trial without a payment method.
func (o *Operator) SetPlan(ctx context.Context, by billing.Actor, id, plan string) (*billing.Account, error) {
	pl := o.Plans.Get(plan)
	if pl == nil {
		return nil, fmt.Errorf("%w: unknown plan %q", billing.ErrInvalid, plan)
	}
	cur, err := o.Account(ctx, id)
	if err != nil {
		return nil, err
	}
	next := cur.Billing
	next.Plan = plan
	maxClients := billing.MaxClients(&next, o.Plans)
	return o.change(ctx, id, func(tx *sqlx.Tx) (bool, error) {
		var found bool
		if err := tx.GetContext(ctx, &found, "SELECT rport_operator_set_plan($1, $2, $3, $4)", id, plan, maxClients, by.Username); err != nil || !found {
			return found, err
		}
		return true, auditBoth(ctx, tx, by, billing.AuditActionPlan, id, map[string]any{"plan": plan, "max_clients": maxClients})
	})
}

// SetStatus implements billing.OperatorService.
func (o *Operator) SetStatus(ctx context.Context, by billing.Actor, id string, c billing.StatusChange) (*billing.Account, error) {
	if err := c.Validate(o.now()); err != nil {
		return nil, err
	}
	var comped any
	if c.CompedUntil != nil {
		comped = c.CompedUntil.Format("2006-01-02")
	}
	return o.change(ctx, id, func(tx *sqlx.Tx) (bool, error) {
		var found bool
		if err := tx.GetContext(ctx, &found, "SELECT rport_operator_set_billing($1, $2, $3, $4::date, $5, $6, $7, $8)",
			id, string(c.Status), c.TrialEndsAt, comped, c.Reason, string(c.ExemptLimits), o.Plans.Free, by.Username); err != nil || !found {
			return found, err
		}
		return true, auditBoth(ctx, tx, by, billing.AuditActionStatus, id, c)
	})
}

// SetOverride implements billing.OperatorService. What ov leaves out keeps its value.
func (o *Operator) SetOverride(ctx context.Context, by billing.Actor, id string, ov billing.Override) (*billing.Account, error) {
	cur, err := o.Account(ctx, id)
	if err != nil {
		return nil, err
	}
	month := billing.MonthStart(o.now())
	until := cur.CapLiftedUntil
	switch {
	case ov.EndCapLift && ov.CapLiftedUntil != nil:
		return nil, fmt.Errorf("%w: give cap_lifted_until or end_cap_lift, not both", billing.ErrInvalid)
	case ov.EndCapLift:
		until = nil
	case ov.CapLiftedUntil != nil:
		if !ov.CapLiftedUntil.After(o.now()) {
			return nil, fmt.Errorf("%w: cap_lifted_until must be in the future", billing.ErrInvalid)
		}
		until = ov.CapLiftedUntil
	}
	var extra int64
	if cur.ExtraTransferPeriod != nil && billing.MonthStart(*cur.ExtraTransferPeriod).Equal(month) {
		extra = cur.ExtraTransferBytes
	}
	if ov.ExtraTransfer != "" {
		b, err := billing.ParseByteSize(ov.ExtraTransfer)
		if err != nil {
			return nil, err
		}
		extra = int64(b)
	}
	var period any
	if extra > 0 {
		period = month.Format("2006-01-02")
	}
	return o.change(ctx, id, func(tx *sqlx.Tx) (bool, error) {
		var found bool
		if err := tx.GetContext(ctx, &found, "SELECT rport_operator_billing_override($1, $2, $3, $4::date, $5, $6, $7)",
			id, until, extra, period, ov.ClearFailOpen, o.Plans.Free, by.Username); err != nil || !found {
			return found, err
		}
		return true, auditBoth(ctx, tx, by, billing.AuditActionOverride, id, ov)
	})
}

// Periods implements billing.OperatorService.
func (o *Operator) Periods(ctx context.Context, id string, state billing.PeriodState) ([]billing.Period, error) {
	if id != "" {
		if _, err := o.Account(ctx, id); err != nil {
			return nil, err
		}
	}
	var idArg, stateArg any
	if id != "" {
		idArg = id
	}
	if state != "" {
		stateArg = string(state)
	}
	out := []billing.Period{}
	err := inSystem(ctx, o.DB, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &out, "SELECT "+periodColumns+" FROM rport_operator_billing_periods($1, $2)", idArg, stateArg)
	})
	if err != nil {
		return nil, fmt.Errorf("billing: %w", err)
	}
	return out, nil
}

// ApprovePeriod implements billing.OperatorService.
func (o *Operator) ApprovePeriod(ctx context.Context, by billing.Actor, id string, start time.Time) error {
	if !validOrganization(id) {
		return billing.ErrNotFound
	}
	start = billing.MonthStart(start)
	return inSystem(ctx, o.DB, func(tx *sqlx.Tx) error {
		var found bool
		if err := tx.GetContext(ctx, &found, "SELECT rport_operator_approve_period($1, $2::date, $3)", id, start.Format("2006-01-02"), by.Username); err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("no held period of %s starting %s: %w", id, start.Format("2006-01"), billing.ErrNotFound)
		}
		return auditBoth(ctx, tx, by, billing.AuditActionApprove, id, map[string]string{"period": start.Format("2006-01")})
	})
}
