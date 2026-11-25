package signup

import (
	"context"
	"time"

	"github.com/openrport/openrport/server/billing"
	"github.com/openrport/openrport/server/operator"
)

// BillingPlans is Plans on billing ([billing] enabled): a new organization gets the trial of the plans file
// ([trial]: its plan for its days, capped while no payment method is on file, then the free plan), or the free plan
// when there is no trial.
type BillingPlans struct {
	Operator billing.OperatorService
	Plans    *billing.Plans
	Now      func() time.Time
}

func (p BillingPlans) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// hasTrial reports whether the plans file gives new organizations a trial.
func hasTrial(plans *billing.Plans) bool {
	t := plans.Trial
	return t.Days > 0 && t.Plan != "" && t.Plan != plans.Free
}

// TrialDays is the length of the trial of a new organization; 0: none.
func (p BillingPlans) TrialDays() int {
	if p.Plans == nil || !hasTrial(p.Plans) {
		return 0
	}
	return p.Plans.Trial.Days
}

// StartOrganization implements Plans. The status is set before the plan, so the device limit the plan sets is the
// trial's while no payment method is on file.
func (p BillingPlans) StartOrganization(ctx context.Context, by operator.Actor, org string) (*PlanStart, error) {
	actor := billing.Actor{Username: by.Username, RemoteIP: by.RemoteIP}
	if !hasTrial(p.Plans) {
		if _, err := p.Operator.SetPlan(ctx, actor, org, p.Plans.Free); err != nil {
			return nil, err
		}
		return &PlanStart{Plan: p.Plans.Free, BillingStatus: string(billing.StatusPaying)}, nil
	}
	end := p.now().Add(time.Duration(p.Plans.Trial.Days) * 24 * time.Hour)
	if _, err := p.Operator.SetStatus(ctx, actor, org, billing.StatusChange{Status: billing.StatusTrial, TrialEndsAt: &end}); err != nil {
		return nil, err
	}
	if _, err := p.Operator.SetPlan(ctx, actor, org, p.Plans.Trial.Plan); err != nil {
		return nil, err
	}
	return &PlanStart{Plan: p.Plans.Trial.Plan, BillingStatus: string(billing.StatusTrial), TrialEndsAt: &end}, nil
}

// DeletionRequested implements Plans. The subscription is canceled by the payment provider integration (billing
// phase 4); plans alone have nothing to close.
func (BillingPlans) DeletionRequested(context.Context, operator.Actor, string) error { return nil }

// BeforeDelete implements Plans: org_billing and billing_periods have no foreign key to tenants, so they stay,
// detached, for the accounting retention.
func (BillingPlans) BeforeDelete(context.Context, string) error { return nil }

// NoPlans is Plans without billing ([billing] off): organizations get no plan and no limits beyond what operators
// set.
type NoPlans struct{}

// StartOrganization implements Plans.
func (NoPlans) StartOrganization(context.Context, operator.Actor, string) (*PlanStart, error) {
	return nil, nil
}

// DeletionRequested implements Plans.
func (NoPlans) DeletionRequested(context.Context, operator.Actor, string) error { return nil }

// BeforeDelete implements Plans.
func (NoPlans) BeforeDelete(context.Context, string) error { return nil }
