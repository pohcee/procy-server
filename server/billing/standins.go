// Package billing is hosted billing (billing and metering design): plans, billing status, the payment provider.
//
// STAND-INS: this file holds minimal, field-compatible copies of the types the billing-plans stream
// (feat/billing-plans) defines: Plan, Overage, GiB, Status, Billing, Period, DayStart and the readers of org_billing
// and billing_periods. Delete this file when merging feat/billing-plans (the interfaces this stream needs from it,
// BillingRecords, ClosedPeriods and PlanCatalog, are in deps.go).
package billing

import "time"

// Overage is what happens past a plan's transfer allowance. (stand-in)
type Overage string

// Overage modes. (stand-in)
const (
	OverageBill Overage = "bill"
	OverageCap  Overage = "cap"
)

// GiB is a binary gigabyte, the unit of overage prices. (stand-in)
const GiB = int64(1) << 30

// Plan is one plan of the plans file. (stand-in: the fields this stream reads)
type Plan struct {
	ID                 string  `toml:"id" json:"id"`
	Name               string  `toml:"name" json:"name"`
	BasePrice          float64 `toml:"base_price" json:"base_price"`
	IncludedDevices    int64   `toml:"included_devices" json:"included_devices"`
	PricePerDevice     float64 `toml:"price_per_device" json:"price_per_device"`
	Overage            Overage `toml:"overage" json:"overage"`
	OveragePricePerGiB float64 `toml:"overage_price_per_gib" json:"overage_price_per_gib"`
	// ProviderPrices maps the parts of the price (base, devices, overage) to the payment provider's price ids.
	ProviderPrices map[string]string `toml:"provider_prices" json:"-"`
}

// Status is an organization's billing status. (stand-in)
type Status string

// Billing statuses. (stand-in)
const (
	StatusPaying Status = "paying"
	StatusTrial  Status = "trial"
	StatusExempt Status = "exempt"
	StatusComped Status = "comped"
)

// Billing is an organization's billing record (org_billing). (stand-in: the fields this stream reads)
type Billing struct {
	Organization           string     `json:"organization" db:"tenant_id"`
	Plan                   string     `json:"plan" db:"plan"`
	Status                 Status     `json:"status" db:"status"`
	TrialEndsAt            *time.Time `json:"trial_ends_at" db:"trial_ends_at"`
	CompedUntil            *time.Time `json:"comped_until" db:"comped_until"`
	HasPaymentMethod       bool       `json:"has_payment_method" db:"has_payment_method"`
	ProviderCustomerID     string     `json:"provider_customer_id,omitempty" db:"provider_customer_id"`
	ProviderSubscriptionID string     `json:"provider_subscription_id,omitempty" db:"provider_subscription_id"`
}

// Charged reports whether the organization is charged on day (a UTC day). (stand-in)
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

// DayStart is the start of t's UTC day. (stand-in)
func DayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// PeriodState is what happens to a closed billing period. (stand-in)
type PeriodState string

// Period states. (stand-in)
const (
	PeriodReady        PeriodState = "ready"
	PeriodHeld         PeriodState = "held"
	PeriodNotCharged   PeriodState = "not_charged"
	PeriodNeverCharged PeriodState = "never_charged"
	PeriodReported     PeriodState = "reported"
)

// Period is a closed billing period of an organization with its frozen quantities (billing_periods). (stand-in:
// the fields this stream reads)
type Period struct {
	Organization    string      `json:"organization" db:"tenant_id"`
	Start           time.Time   `json:"start" db:"period_start"`
	End             time.Time   `json:"end" db:"period_end"`
	Plan            string      `json:"plan" db:"plan"`
	Status          Status      `json:"status" db:"billing_status"`
	BillableDevices int64       `json:"billable_devices" db:"billable_devices"`
	ExtraDevices    int64       `json:"extra_devices" db:"extra_devices"`
	TransferBytes   int64       `json:"transfer_bytes" db:"transfer_bytes"`
	AllowanceBytes  int64       `json:"allowance_bytes" db:"allowance_bytes"`
	OverageBytes    int64       `json:"overage_bytes" db:"overage_bytes"`
	OverageGiB      int64       `json:"overage_gib" db:"overage_gib"`
	TotalCents      int64       `json:"total_cents" db:"total_cents"`
	State           PeriodState `json:"state" db:"state"`
	ClosedAt        time.Time   `json:"closed_at" db:"closed_at"`
	ReportedAt      *time.Time  `json:"reported_at" db:"reported_at"`
}
