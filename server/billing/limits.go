package billing

import (
	"fmt"
	"time"
)

// Fail-open guards of the enforcement (Limits and protections).
const (
	// MaxUsageAge: the enforcement refuses to cap while the newest usage it sees is older (metering stalled).
	MaxUsageAge = 2 * time.Hour
	// DefaultImplausibleFactor: it refuses to cap an organization whose transfer in the last hour is more than
	// this many times its average hour of the period so far ([billing] implausible_growth_factor).
	DefaultImplausibleFactor = 20.0
	// implausibleFloor: hours below this are never implausible (a first big download is normal).
	implausibleFloor = 10 * GiB
)

// CurrentUsage is what an organization used in the current period so far.
type CurrentUsage struct {
	// TransferBytes is the transfer since the period started (or billing started, in the first month).
	TransferBytes int64
	// DeviceDays and Days give the billable devices so far: ceil(DeviceDays / Days).
	DeviceDays int64
	Days       int64
	// LastHourBytes is the transfer of the newest complete hour, AvgHourBytes the average hour before it in the
	// period (for the implausible-growth guard).
	LastHourBytes int64
	AvgHourBytes  float64
	// NewestUsage is the newest hour of usage of any organization (for the stale-metering guard).
	NewestUsage time.Time
}

// BillableDevices are the billable devices so far.
func (u CurrentUsage) BillableDevices() int64 {
	if u.Days <= 0 {
		return 0
	}
	return (u.DeviceDays + u.Days - 1) / u.Days
}

// EnforceOptions are the settings of the enforcement.
type EnforceOptions struct {
	// ImplausibleFactor (0: DefaultImplausibleFactor).
	ImplausibleFactor float64
}

// Policy is the transfer policy in force for an organization in the current period: what the enforcement upkeep
// writes to org_billing and device processes read through device_transfer_policy().
type Policy struct {
	Organization string        `json:"organization"`
	Plan         string        `json:"plan"`
	State        TransferState `json:"transfer_state"`
	// AllowanceBytes is the allowance of the period so far (prorated in the first month, the trial's ceiling for
	// a trial without a payment method), UsedBytes the transfer so far.
	AllowanceBytes int64 `json:"allowance_bytes"`
	UsedBytes      int64 `json:"used_bytes"`
	// SoftCapBytes is where the soft cap starts; HardCapBytes where the hard cap starts (0: no hard cap).
	SoftCapBytes int64 `json:"soft_cap_bytes"`
	HardCapBytes int64 `json:"hard_cap_bytes"`
	// OverageCents is the estimated overage of the period so far.
	OverageCents int64 `json:"overage_cents"`
	// TunnelRate is the bandwidth of each tunnel connection in each direction (0: unlimited); ThrottleRate the
	// one past the hard cap.
	TunnelRate   RateLimit `json:"tunnel_rate"`
	ThrottleRate RateLimit `json:"throttle_rate"`
	// MaxClients is the device limit the plan (or a trial without a payment method) sets; nil: unlimited.
	MaxClients *int `json:"max_clients"`
	// FailOpen says why the hard cap was not applied although reached (metering stale or implausible).
	FailOpen string `json:"fail_open,omitempty"`
}

// SoftCapReached is the API flag of the soft cap.
func (p Policy) SoftCapReached() bool { return p.State != TransferNormal }

// EffectivePlan returns the plan an organization's limits come from: its own (or the free plan when unknown).
func EffectivePlan(b *Billing, plans *Plans) *Plan {
	if pl := plans.Get(b.Plan); pl != nil {
		return pl
	}
	return plans.FreePlan()
}

// MaxClients is the device limit of b on its plan: the trial's ceiling for a trial without a payment method.
func MaxClients(b *Billing, plans *Plans) *int {
	pl := EffectivePlan(b, plans)
	if b.CardlessTrial() && plans.Trial.MaxClients != nil {
		if pl.MaxClients == nil || *plans.Trial.MaxClients < *pl.MaxClients {
			return plans.Trial.MaxClients
		}
	}
	return pl.MaxClients
}

// Evaluate computes the policy of b for the period starting at the month of now, from what it used so far.
//
//   - Allowance: the plan's base plus per device × billable devices so far, prorated by the share of the month
//     billing covers, plus an operator's extra transfer; a trial without a payment method gets the trial's
//     ceiling instead (when smaller) and is capped.
//   - Soft cap at the plan's soft_cap percent; past 100% a plan that bills overage is "over"; the hard cap is the
//     plan's hard_cap on capped plans (Free, trials without a card) or the organization's spending limit on plans
//     that bill overage. A paying organization on a plan that bills overage is never capped without one.
//   - An exempt organization with exempt_limits "none" has no limits at all; an operator's cap lift (until a
//     time) keeps the hard cap off.
//   - Fail open: the hard cap is not applied while the newest usage is older than MaxUsageAge or the last hour is
//     implausible; FailOpen says why.
func Evaluate(b *Billing, plans *Plans, u CurrentUsage, o EnforceOptions, now time.Time) Policy {
	pl := EffectivePlan(b, plans)
	p := Policy{Organization: b.Organization, Plan: pl.ID, UsedBytes: u.TransferBytes, State: TransferNormal,
		MaxClients: MaxClients(b, plans)}
	if b.Status == StatusExempt && b.ExemptLimits == ExemptLimitsNone {
		return p
	}
	p.TunnelRate, p.ThrottleRate = pl.TunnelRateLimit, pl.ThrottleRate

	start := MonthStart(now)
	from := start
	if s := DayStart(b.StartedAt); s.After(from) {
		from = s
	}
	proration := float64(daysBetween(from, start.AddDate(0, 1, 0))) / float64(daysBetween(start, start.AddDate(0, 1, 0)))
	p.AllowanceBytes = int64(float64(pl.Allowance(u.BillableDevices())) * proration)
	if b.ExtraTransferPeriod != nil && MonthStart(*b.ExtraTransferPeriod).Equal(start) {
		p.AllowanceBytes += b.ExtraTransferBytes
	}
	overage, hardCapPercent := pl.Overage, pl.HardCapPercent
	if b.CardlessTrial() {
		if plans.Trial.Transfer > 0 && int64(plans.Trial.Transfer) < p.AllowanceBytes {
			p.AllowanceBytes = int64(plans.Trial.Transfer)
		}
		overage, hardCapPercent = OverageCap, DefaultHardCapPercent
	}
	p.SoftCapBytes = p.AllowanceBytes * int64(pl.SoftCapPercent) / 100
	if overage == OverageCap {
		if hardCapPercent == 0 {
			hardCapPercent = DefaultHardCapPercent
		}
		p.HardCapBytes = max(p.AllowanceBytes*int64(hardCapPercent)/100, 1)
	}
	if over := u.TransferBytes - p.AllowanceBytes; over > 0 && overage == OverageBill {
		p.OverageCents = (over + GiB - 1) / GiB * Cents(pl.OveragePricePerGiB)
	}
	switch {
	case p.HardCapBytes > 0 && u.TransferBytes >= p.HardCapBytes:
		p.State = TransferCapped
	case overage == OverageBill && b.SpendingLimitCents != nil && p.OverageCents >= *b.SpendingLimitCents && u.TransferBytes > p.AllowanceBytes:
		p.State = TransferCapped
		p.HardCapBytes = u.TransferBytes
	case u.TransferBytes >= p.AllowanceBytes && p.AllowanceBytes > 0:
		p.State = TransferOver
	case u.TransferBytes >= p.SoftCapBytes && p.SoftCapBytes > 0:
		p.State = TransferSoft
	}
	if overage == OverageBill && b.SpendingLimitCents != nil && p.State != TransferCapped {
		// where the spending limit is reached: the allowance plus the GiB it buys
		if price := Cents(pl.OveragePricePerGiB); price > 0 {
			p.HardCapBytes = p.AllowanceBytes + *b.SpendingLimitCents/price*GiB
		}
	}
	if p.State != TransferCapped {
		return p
	}
	if b.CapLiftedUntil != nil && now.Before(*b.CapLiftedUntil) {
		p.State = TransferOver
		return p
	}
	factor := o.ImplausibleFactor
	if factor <= 0 {
		factor = DefaultImplausibleFactor
	}
	switch {
	case u.NewestUsage.IsZero() || now.Sub(u.NewestUsage) > MaxUsageAge:
		p.FailOpen = fmt.Sprintf("metering is stale: the newest usage is from %s", u.NewestUsage.UTC().Format(time.RFC3339))
	case u.LastHourBytes > implausibleFloor && float64(u.LastHourBytes) > factor*u.AvgHourBytes:
		p.FailOpen = fmt.Sprintf("implausible transfer: %d bytes in the last hour, %.0f times the period's average hour",
			u.LastHourBytes, float64(u.LastHourBytes)/max(u.AvgHourBytes, 1))
	}
	if p.FailOpen != "" {
		p.State = TransferOver
	}
	return p
}
