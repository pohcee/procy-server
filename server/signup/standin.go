package signup

import (
	"context"
	"time"

	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/operator"
)

// StandInPlans is Plans until billing (the billing-plans stream) provides plans and billing statuses: a new
// organization gets [signup] trial_device_limit as its device limit (tenants.max_clients), and the plan names of
// [signup] are only reported. Transfer caps and the end of the trial need the plans. Nothing is closed on deletion.
type StandInPlans struct {
	Orgs   operator.Service
	Config chconfig.SignupConfig
	Now    func() time.Time
}

// StartOrganization implements Plans.
func (p StandInPlans) StartOrganization(ctx context.Context, by operator.Actor, org string) (*PlanStart, error) {
	limit := p.Config.TrialDeviceLimit
	if _, err := p.Orgs.SetDeviceLimit(ctx, by, org, &limit); err != nil {
		return nil, err
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	if days := p.Config.TrialDaysOrDefault(); days > 0 && p.Config.TrialPlan != "" {
		end := now().Add(time.Duration(days) * 24 * time.Hour)
		return &PlanStart{Plan: p.Config.TrialPlan, BillingStatus: "trial", TrialEndsAt: &end}, nil
	}
	return &PlanStart{Plan: p.Config.DefaultPlan, BillingStatus: "paying"}, nil
}

// DeletionRequested implements Plans.
func (StandInPlans) DeletionRequested(context.Context, operator.Actor, string) error { return nil }

// BeforeDelete implements Plans.
func (StandInPlans) BeforeDelete(context.Context, string) error { return nil }
