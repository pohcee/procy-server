package billingprovider

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/server/billing"
	pgbilling "github.com/pohcee/procy-server/server/storage/postgres/billing"
	"github.com/pohcee/procy-server/share/tenant"
)

// Records reads the billing statuses from org_billing (migration 1450) for the provider service.
type Records struct {
	Store pgbilling.Store
}

var _ billing.RecordReader = Records{}

// Billing implements billing.RecordReader: the organization's record, or the default (free plan, paying).
func (r Records) Billing(ctx context.Context, org string) (*billing.Billing, error) {
	a, err := r.Store.Account(ctx, tenant.ID(org))
	if err != nil {
		return nil, err
	}
	return &a.Billing, nil
}

// Periods lists the closed periods ready to be charged (billing_periods).
type Periods struct {
	Operator *pgbilling.Operator
}

var _ billing.ClosedPeriods = Periods{}

// ClosedSince implements billing.ClosedPeriods: the periods in state ready that started at or after since's
// month, oldest first.
func (p Periods) ClosedSince(ctx context.Context, since time.Time) ([]billing.Period, error) {
	all, err := p.Operator.Periods(ctx, "", billing.PeriodReady)
	if err != nil {
		return nil, err
	}
	from := billing.MonthStart(since)
	out := make([]billing.Period, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		if !all[i].Start.Before(from) {
			out = append(out, all[i])
		}
	}
	return out, nil
}

// Sync passes what the provider learned to org_billing (rport_billing_provider_state, rport_billing_period_reported).
type Sync struct {
	DB *sqlx.DB
	// FreePlan is the plan of an organization without a billing record yet.
	FreePlan string
	// Plans is the catalog: a plan bought gets its device limit from it (as an operator's SetPlan).
	Plans *billing.Plans
	// By names the writer in org_billing.updated_by.
	By string
}

var _ billing.Sync = Sync{}

// ProviderState implements billing.Sync.
func (s Sync) ProviderState(ctx context.Context, org, customerID, subscriptionID string, hasPaymentMethod bool, plan string) error {
	if err := (Store{DB: s.DB}).inCtx(ctx, func(tx *sqlx.Tx) error {
		var maxClients *int
		if plan != "" && s.Plans != nil {
			var err error
			if maxClients, err = pgbilling.PlanMaxClients(ctx, tx, s.Plans, org, plan, hasPaymentMethod); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "SELECT rport_billing_provider_state($1, $2, $3, $4, $5, $6, $7, $8)",
			org, customerID, subscriptionID, hasPaymentMethod, plan, maxClients, s.FreePlan, s.By)
		return err
	}); err != nil {
		return fmt.Errorf("billing: provider state of %s: %w", org, err)
	}
	return nil
}

// PeriodReported implements billing.Sync.
func (s Sync) PeriodReported(ctx context.Context, org string, start, _ time.Time) error {
	if err := (Store{DB: s.DB}).inCtx(ctx, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, "SELECT rport_billing_period_reported($1, $2::date)", org, start.UTC().Format("2006-01-02"))
		return err
	}); err != nil {
		return fmt.Errorf("billing: period %s of %s reported: %w", start.Format("2006-01"), org, err)
	}
	return nil
}
