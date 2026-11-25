package billing

import (
	"context"
	"time"
)

// RecordReader reads organizations' billing records (billing-plans' org_billing store). A missing record is
// (nil, nil): the organization is on the free plan, paying.
type RecordReader interface {
	Billing(ctx context.Context, org string) (*Billing, error)
}

// ClosedPeriods lists the billing periods closed at or after a time (billing-plans' period close), oldest first.
type ClosedPeriods interface {
	ClosedSince(ctx context.Context, since time.Time) ([]Period, error)
}

// PlanCatalog finds plans by id (billing-plans' plans file).
type PlanCatalog interface {
	Get(id string) *Plan
}
