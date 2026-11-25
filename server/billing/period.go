package billing

import (
	"time"
)

// PeriodState is what happens to a closed billing period (billing_periods.state).
type PeriodState string

const (
	// PeriodReady is to be charged: the payment provider reports its quantities and sets PeriodReported.
	PeriodReady PeriodState = "ready"
	// PeriodHeld has overage above [billing] overage_review_above: an operator approves it (then PeriodReady).
	PeriodHeld PeriodState = "held"
	// PeriodNotCharged is a period with nothing to charge: a trial or comped for the whole period, or a total of 0.
	PeriodNotCharged PeriodState = "not_charged"
	// PeriodNeverCharged is a period of an exempt organization: metered and shown, never sent to a provider.
	PeriodNeverCharged PeriodState = "never_charged"
	// PeriodReported is set by the payment provider once it acknowledged the period's usage.
	PeriodReported PeriodState = "reported"
)

// SettleDelay is how long after a period ends it is closed: late usage (a node's outbox shipped late) still counts.
const SettleDelay = 2 * time.Hour

// Period is a closed billing period of an organization with its frozen quantities (billing_periods). Periods are
// calendar months in UTC. Usage arriving after the close is shown in the usage report but never billed.
type Period struct {
	Organization string `json:"organization" db:"tenant_id"`
	// Start and End are the month: [Start, End).
	Start time.Time `json:"start" db:"period_start"`
	End   time.Time `json:"end" db:"period_end"`
	// BilledFrom is the first day the quantities cover: Start, or a later day in the organization's first month
	// (billing started), after a trial or after a comped date. The prices are prorated by Proration.
	BilledFrom time.Time `json:"billed_from" db:"billed_from"`
	// Proration is the share of the month [BilledFrom, End) covers (1 for a whole month).
	Proration float64 `json:"proration" db:"proration"`
	Plan      string  `json:"plan" db:"plan"`
	// Status is the organization's billing status at the close.
	Status Status `json:"status" db:"billing_status"`
	// BillableDevices is the average of the daily device counts over [BilledFrom, End), rounded up.
	BillableDevices int64 `json:"billable_devices" db:"billable_devices"`
	// PeakDevices is the highest daily device count.
	PeakDevices int64 `json:"peak_devices" db:"peak_devices"`
	DeviceDays  int64 `json:"device_days" db:"device_days"`
	// ExtraDevices are the billable devices above the plan's included devices.
	ExtraDevices         int64 `json:"extra_devices" db:"extra_devices"`
	TunnelBytesToUser    int64 `json:"tunnel_bytes_to_user" db:"tunnel_bytes_to_user"`
	TunnelBytesToDevice  int64 `json:"tunnel_bytes_to_device" db:"tunnel_bytes_to_device"`
	UploadBytesToDevices int64 `json:"upload_bytes_to_devices" db:"upload_bytes_to_devices"`
	// TransferBytes is the sum of the three: what plans count as transfer.
	TransferBytes  int64 `json:"transfer_bytes" db:"transfer_bytes"`
	AllowanceBytes int64 `json:"allowance_bytes" db:"allowance_bytes"`
	// OverageBytes is the transfer past the allowance on a plan that bills overage (0 on capped plans).
	OverageBytes int64 `json:"overage_bytes" db:"overage_bytes"`
	// OverageGiB is OverageBytes in whole GiB, rounded up: the quantity charged.
	OverageGiB int64 `json:"overage_gib" db:"overage_gib"`
	// Amounts in USD cents at the plan's prices. Computed for every period (also exempt ones, as the list price).
	BaseCents    int64 `json:"base_cents" db:"base_cents"`
	DeviceCents  int64 `json:"device_cents" db:"device_cents"`
	OverageCents int64 `json:"overage_cents" db:"overage_cents"`
	TotalCents   int64 `json:"total_cents" db:"total_cents"`

	State      PeriodState `json:"state" db:"state"`
	ClosedAt   time.Time   `json:"closed_at" db:"closed_at"`
	ApprovedBy *string     `json:"approved_by" db:"approved_by"`
	ApprovedAt *time.Time  `json:"approved_at" db:"approved_at"`
	// ReportedAt is set by the payment provider (PeriodReported).
	ReportedAt *time.Time `json:"reported_at" db:"reported_at"`
}

// PeriodUsage is what an organization used in the span of a period being closed.
type PeriodUsage struct {
	// DailyDevices are the devices that had a session on each UTC day of the span (usage_device_days).
	DailyDevices         []int64
	TunnelBytesToUser    int64
	TunnelBytesToDevice  int64
	UploadBytesToDevices int64
}

// PeriodSpan returns (for b as it applied at the end of the period, see Billing.AsOf) the first day the period starting start is billed from for b, and whether any of it is
// charged: from the later of start, the day billing started, the day a trial ended and the day after a comped date.
// Exempt organizations are never charged; for them, and for periods not charged at all, from is the first day
// billing covers (for the quantities shown).
func PeriodSpan(b *Billing, start time.Time) (from time.Time, charged bool) {
	start = MonthStart(start)
	end := start.AddDate(0, 1, 0)
	from = start
	if s := DayStart(b.StartedAt); s.After(from) {
		from = s
	}
	covered := from
	if b.Status == StatusExempt {
		return covered, false
	}
	if b.Status == StatusPaying && b.PreviousStatus != nil && *b.PreviousStatus == StatusExempt && b.StatusSince != nil {
		// exempt until then: charged from that day
		if d := DayStart(*b.StatusSince); d.After(from) {
			from = d
		}
	}
	if b.TrialEndsAt != nil && (b.Status == StatusTrial || b.Status == StatusPaying) {
		// a trial is not charged before the day it ends; the day it ends is charged
		if d := DayStart(*b.TrialEndsAt); d.After(from) {
			from = d
		}
		if b.Status == StatusTrial {
			return covered, false
		}
	}
	if b.CompedUntil != nil && (b.Status == StatusComped || b.Status == StatusPaying) {
		if d := DayStart(*b.CompedUntil).AddDate(0, 0, 1); d.After(from) {
			from = d
		}
		if b.Status == StatusComped && !from.Before(end) {
			return covered, false
		}
	}
	if !from.Before(end) {
		return covered, false
	}
	return from, true
}

// ClosePeriod freezes the quantities of the period starting start for b on plan, from what it used in the span
// from PeriodSpan: billable devices (average of the daily counts rounded up), transfer, allowance (prorated, plus an
// operator's extra transfer for the period), overage on plans that bill it, and the amounts. The state is
// never_charged for exempt organizations, not_charged when nothing is charged, held when the overage exceeds
// reviewAboveCents (> 0), ready otherwise.
func ClosePeriod(b *Billing, plan *Plan, start time.Time, u PeriodUsage, reviewAboveCents int64, now time.Time) Period {
	start = MonthStart(start)
	end := start.AddDate(0, 1, 0)
	from, charged := PeriodSpan(b, start)
	p := Period{
		Organization: b.Organization,
		Start:        start,
		End:          end,
		BilledFrom:   from,
		Plan:         plan.ID,
		Status:       b.Status,
		ClosedAt:     now.UTC(),
	}
	monthDays := daysBetween(start, end)
	spanDays := daysBetween(from, end)
	p.Proration = float64(spanDays) / float64(monthDays)
	for _, n := range u.DailyDevices {
		p.DeviceDays += n
		p.PeakDevices = max(p.PeakDevices, n)
	}
	if spanDays > 0 {
		p.BillableDevices = (p.DeviceDays + spanDays - 1) / spanDays
	}
	p.ExtraDevices = max(p.BillableDevices-plan.IncludedDevices, 0)
	p.TunnelBytesToUser, p.TunnelBytesToDevice, p.UploadBytesToDevices = u.TunnelBytesToUser, u.TunnelBytesToDevice, u.UploadBytesToDevices
	p.TransferBytes = u.TunnelBytesToUser + u.TunnelBytesToDevice + u.UploadBytesToDevices
	p.AllowanceBytes = int64(float64(plan.Allowance(p.BillableDevices)) * p.Proration)
	if b.ExtraTransferPeriod != nil && MonthStart(*b.ExtraTransferPeriod).Equal(start) {
		p.AllowanceBytes += b.ExtraTransferBytes
	}
	if plan.Overage == OverageBill && p.TransferBytes > p.AllowanceBytes {
		p.OverageBytes = p.TransferBytes - p.AllowanceBytes
		p.OverageGiB = (p.OverageBytes + GiB - 1) / GiB
	}
	p.BaseCents = int64(float64(Cents(plan.BasePrice))*p.Proration + 0.5)
	p.DeviceCents = int64(float64(p.ExtraDevices*Cents(plan.PricePerDevice))*p.Proration + 0.5)
	p.OverageCents = p.OverageGiB * Cents(plan.OveragePricePerGiB)
	p.TotalCents = p.BaseCents + p.DeviceCents + p.OverageCents
	switch {
	case b.Status == StatusExempt:
		p.State = PeriodNeverCharged
	case !charged || p.TotalCents == 0:
		p.State = PeriodNotCharged
	case reviewAboveCents > 0 && p.OverageCents > reviewAboveCents:
		p.State = PeriodHeld
	default:
		p.State = PeriodReady
	}
	return p
}

// daysBetween is the number of UTC days in [from, to).
func daysBetween(from, to time.Time) int64 {
	if !to.After(from) {
		return 0
	}
	return int64(DayStart(to).Sub(DayStart(from)).Hours()+0.5) / 24
}
