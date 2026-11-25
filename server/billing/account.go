package billing

import (
	"context"
	"errors"
	"time"

	"github.com/openrport/openrport/share/tenant"
)

// ErrNotFound: no such organization, or no such period.
var ErrNotFound = errors.New("not found")

// Audit log application and actions of billing changes, written to the system tenant's audit log and to the
// organization's own.
const (
	AuditApplication     = "billing"
	AuditActionPlan      = "set_plan"
	AuditActionStatus    = "set_status"
	AuditActionOverride  = "override"
	AuditActionApprove   = "approve_period"
	AuditActionSpending  = "set_spending_limit"
	AuditActionTrialEnd  = "trial_ended"
	AuditActionCompedEnd = "comped_ended"
)

// Actor is who makes a billing change, for the audit log.
type Actor struct {
	Username string
	RemoteIP string
}

// Account is an organization's billing record with the transfer policy the enforcement upkeep last wrote and the
// details of its plan.
type Account struct {
	Billing
	Policy Policy `json:"policy"`
	// SoftCapReached is the API flag of the soft cap (and of every state past it).
	SoftCapReached bool  `json:"soft_cap_reached"`
	PlanDetails    *Plan `json:"plan_details"`
}

// Override is an operator's change of the limits of the current period.
type Override struct {
	// CapLiftedUntil lifts the hard cap until then; nil ends a lift.
	CapLiftedUntil *time.Time `json:"cap_lifted_until"`
	// ExtraTransfer raises the allowance of the current period (a size such as "50GiB"; "" or "0": none).
	ExtraTransfer string `json:"extra_transfer"`
}

// OperatorService is what platform operators do with billing (operator API and CLI). Every change is audited.
type OperatorService interface {
	// Accounts returns every organization's account.
	Accounts(ctx context.Context) ([]Account, error)
	// Account returns one organization's account (ErrNotFound).
	Account(ctx context.Context, id string) (*Account, error)
	// SetPlan puts the organization on plan; its device limit follows the plan.
	SetPlan(ctx context.Context, by Actor, id, plan string) (*Account, error)
	// SetStatus sets the billing status.
	SetStatus(ctx context.Context, by Actor, id string, c StatusChange) (*Account, error)
	// SetOverride lifts the hard cap or raises the allowance for the current period.
	SetOverride(ctx context.Context, by Actor, id string, o Override) (*Account, error)
	// Periods returns the closed periods of id (every organization when id is ""), newest first, only those in
	// state when it is set.
	Periods(ctx context.Context, id string, state PeriodState) ([]Period, error)
	// ApprovePeriod releases a held period (overage review).
	ApprovePeriod(ctx context.Context, by Actor, id string, start time.Time) error
}

// OrgService is what an organization's administrators see and set (GET /billing).
type OrgService interface {
	Account(ctx context.Context, tid tenant.ID) (*Account, error)
	Periods(ctx context.Context, tid tenant.ID) ([]Period, error)
	// SetSpendingLimit sets the most overage per period (cents; nil removes the limit) on a plan that bills overage.
	SetSpendingLimit(ctx context.Context, tid tenant.ID, by Actor, cents *int64) (*Account, error)
}

// DefaultAccount is the account of an organization without a billing record: the free plan, paying.
func DefaultAccount(id string, plans *Plans) *Account {
	return &Account{Billing: Billing{Organization: id, Plan: plans.Free, Status: StatusPaying, ExemptLimits: ExemptLimitsPlan,
		TransferState: TransferNormal}, Policy: Policy{Organization: id, Plan: plans.Free, State: TransferNormal}, PlanDetails: plans.FreePlan()}
}

// Notifier sends a billing notification (email) to an organization's administrators.
type Notifier interface {
	Notify(ctx context.Context, tid tenant.ID, recipients []string, subject, body string) error
}

// EnforceInterval is how often the billing upkeep computes the transfer policies and closes periods (a variable
// for tests).
var EnforceInterval = 5 * time.Minute
