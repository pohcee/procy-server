package billing

import (
	"fmt"
	"time"
)

// Status is an organization's billing status, set by operators (org_billing.status).
type Status string

const (
	// StatusPaying is charged on its plan (the default; an organization on the free plan pays nothing).
	StatusPaying Status = "paying"
	// StatusTrial is not charged until TrialEndsAt; then it becomes paying with a payment method on file, or
	// moves to the free plan without one.
	StatusTrial Status = "trial"
	// StatusExempt is metered and never charged (internal and partner organizations); a reason is required and
	// nothing is sent to the payment provider.
	StatusExempt Status = "exempt"
	// StatusComped is not charged until CompedUntil (a UTC date, included); then as a trial that ends.
	StatusComped Status = "comped"
)

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusPaying, StatusTrial, StatusExempt, StatusComped:
		return true
	}
	return false
}

// ExemptLimits is whether the plan's limits apply to an exempt organization.
type ExemptLimits string

const (
	// ExemptLimitsPlan applies the plan's limits (the default).
	ExemptLimitsPlan ExemptLimits = "plan"
	// ExemptLimitsNone applies no transfer limits and no rate limits.
	ExemptLimitsNone ExemptLimits = "none"
)

// TransferState is where an organization's transfer stands in the current period (org_billing.transfer_state).
type TransferState string

const (
	// TransferNormal is below the soft cap.
	TransferNormal TransferState = "normal"
	// TransferSoft is at or above the soft cap: the administrators are notified, nothing is limited.
	TransferSoft TransferState = "soft"
	// TransferOver is past the allowance on a plan that bills overage: nothing is limited.
	TransferOver TransferState = "over"
	// TransferCapped is at the hard cap: tunnels are throttled to the throttle rate, new tunnels and uploads are
	// refused. Device sessions are never closed.
	TransferCapped TransferState = "capped"
)

// Billing is an organization's billing record (org_billing). An organization without one is on the free plan,
// paying.
type Billing struct {
	Organization string `json:"organization" db:"tenant_id"`
	Plan         string `json:"plan" db:"plan"`
	Status       Status `json:"status" db:"status"`
	// TrialEndsAt is when a trial ends (StatusTrial).
	TrialEndsAt *time.Time `json:"trial_ends_at" db:"trial_ends_at"`
	// CompedUntil is the last day (UTC, included) a comped organization is not charged (StatusComped).
	CompedUntil *time.Time `json:"comped_until" db:"comped_until"`
	// Reason is why the status was set (required for exempt).
	Reason string `json:"reason" db:"reason"`
	// ExemptLimits is whether the plan's limits apply to an exempt organization.
	ExemptLimits ExemptLimits `json:"exempt_limits" db:"exempt_limits"`
	// SpendingLimitCents is the most overage (USD cents) per period the organization's administrators allow on
	// a plan that bills overage; reaching it is the hard cap. nil: no limit.
	SpendingLimitCents *int64 `json:"spending_limit_cents" db:"spending_limit_cents"`
	// HasPaymentMethod is set by the payment provider (a card is on file).
	HasPaymentMethod bool `json:"has_payment_method" db:"has_payment_method"`
	// ProviderCustomerID and ProviderSubscriptionID are the payment provider's records (never for exempt).
	ProviderCustomerID     string `json:"provider_customer_id,omitempty" db:"provider_customer_id"`
	ProviderSubscriptionID string `json:"provider_subscription_id,omitempty" db:"provider_subscription_id"`
	// CapLiftedUntil is an operator override: no hard cap until then.
	CapLiftedUntil *time.Time `json:"cap_lifted_until" db:"cap_lifted_until"`
	// ExtraTransferBytes raises the allowance of the period starting ExtraTransferPeriod (operator override).
	ExtraTransferBytes  int64      `json:"extra_transfer_bytes" db:"extra_transfer_bytes"`
	ExtraTransferPeriod *time.Time `json:"extra_transfer_period" db:"extra_transfer_period"`
	// StartedAt is when the organization's billing started (the first period is prorated from it).
	StartedAt time.Time `json:"started_at" db:"started_at"`
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
	UpdatedBy string    `json:"updated_by" db:"updated_by"`

	// The policy in force, written by the enforcement upkeep.
	TransferState   TransferState `json:"transfer_state" db:"transfer_state"`
	TransferStateAt *time.Time    `json:"transfer_state_at" db:"transfer_state_at"`
}

// Charged reports whether the organization is charged on day (a UTC day): paying, a trial that ended before it, a
// comped organization after its last comped day. Exempt is never charged.
func (b *Billing) Charged(day time.Time) bool {
	day = DayStart(day)
	switch b.Status {
	case StatusPaying:
		return true
	case StatusTrial:
		return b.TrialEndsAt != nil && !day.Before(DayStart(*b.TrialEndsAt))
	case StatusComped:
		return b.CompedUntil != nil && day.After(DayStart(*b.CompedUntil))
	}
	return false
}

// CardlessTrial reports whether the trial caps apply: a trial without a payment method.
func (b *Billing) CardlessTrial() bool { return b.Status == StatusTrial && !b.HasPaymentMethod }

// StatusChange is what an operator sets with the billing status.
type StatusChange struct {
	Status Status `json:"status"`
	// TrialEndsAt is required for a trial.
	TrialEndsAt *time.Time `json:"trial_ends_at,omitempty"`
	// CompedUntil (a date) is required for comped.
	CompedUntil *time.Time `json:"comped_until,omitempty"`
	// Reason is required for exempt.
	Reason string `json:"reason,omitempty"`
	// ExemptLimits applies to exempt (default "plan").
	ExemptLimits ExemptLimits `json:"exempt_limits,omitempty"`
}

// Validate checks c and fills its defaults.
func (c *StatusChange) Validate(now time.Time) error {
	if !c.Status.Valid() {
		return fmt.Errorf("%w: status must be paying, trial, exempt or comped, not %q", ErrInvalid, c.Status)
	}
	if len(c.Reason) > 500 {
		return fmt.Errorf("%w: the reason must be at most 500 characters", ErrInvalid)
	}
	if c.Status != StatusTrial {
		if c.TrialEndsAt != nil {
			return fmt.Errorf("%w: trial_ends_at is only for a trial", ErrInvalid)
		}
	} else if c.TrialEndsAt == nil || !c.TrialEndsAt.After(now) {
		return fmt.Errorf("%w: a trial needs trial_ends_at in the future", ErrInvalid)
	}
	if c.Status != StatusComped {
		if c.CompedUntil != nil {
			return fmt.Errorf("%w: comped_until is only for comped", ErrInvalid)
		}
	} else {
		if c.CompedUntil == nil || DayStart(*c.CompedUntil).Before(DayStart(now)) {
			return fmt.Errorf("%w: comped needs comped_until, a date not in the past", ErrInvalid)
		}
		d := DayStart(*c.CompedUntil)
		c.CompedUntil = &d
	}
	if c.Status == StatusExempt {
		if c.Reason == "" {
			return fmt.Errorf("%w: exempt needs a reason", ErrInvalid)
		}
		switch c.ExemptLimits {
		case "":
			c.ExemptLimits = ExemptLimitsPlan
		case ExemptLimitsPlan, ExemptLimitsNone:
		default:
			return fmt.Errorf("%w: exempt_limits must be \"plan\" or \"none\"", ErrInvalid)
		}
	} else if c.ExemptLimits != "" {
		return fmt.Errorf("%w: exempt_limits is only for exempt", ErrInvalid)
	}
	return nil
}
