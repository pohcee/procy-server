package billing

import (
	"context"
	"time"
)

// ProviderAccount is an organization's record at the payment provider (billing_provider_accounts): its customer and
// subscription and its dunning state.
type ProviderAccount struct {
	Organization string `json:"organization" db:"tenant_id"`
	Provider     string `json:"provider" db:"provider"`
	CustomerID   string `json:"customer_id" db:"customer_id"`
	// BillingEmail is the email the customer was created with (the administrator who started checkout).
	BillingEmail       string             `json:"billing_email" db:"billing_email"`
	SubscriptionID     string             `json:"subscription_id" db:"subscription_id"`
	SubscriptionStatus SubscriptionStatus `json:"subscription_status" db:"subscription_status"`
	// SubscriptionEventAt is the creation time of the newest provider event applied to the subscription.
	SubscriptionEventAt *time.Time `json:"-" db:"subscription_event_at"`
	PriceIDs            []string   `json:"price_ids" db:"-"`
	HasPaymentMethod    bool       `json:"has_payment_method" db:"has_payment_method"`
	Dunning
	UpdatedAt time.Time `json:"updated_at" db:"updated_at"`
	UpdatedBy string    `json:"updated_by" db:"updated_by"`
}

// Access is the organization's access from its dunning state.
func (a *ProviderAccount) Access() Access {
	if a == nil {
		return AccessFull
	}
	return a.Dunning.Access()
}

// ReportState is the state of a usage report.
type ReportState string

const (
	// ReportSent: the provider accepted the record.
	ReportSent ReportState = "sent"
	// ReportConfirmed: the provider's summary matches the quantity.
	ReportConfirmed ReportState = "confirmed"
	// ReportMismatch: the provider's summary differs from the quantity; operators look (nothing is corrected
	// automatically).
	ReportMismatch ReportState = "mismatch"
	// ReportFailed: the last send failed; it is retried.
	ReportFailed ReportState = "failed"
	// ReportExpired: the usage became too old for the provider before it could be sent; operators invoice it by
	// hand.
	ReportExpired ReportState = "expired"
)

// UsageReport is one quantity of one closed period reported to the provider (billing_usage_reports).
type UsageReport struct {
	Organization string      `json:"organization" db:"tenant_id"`
	PeriodStart  time.Time   `json:"period_start" db:"period_start"`
	PeriodEnd    time.Time   `json:"period_end" db:"period_end"`
	Metric       string      `json:"metric" db:"metric"`
	Identifier   string      `json:"identifier" db:"identifier"`
	CustomerID   string      `json:"customer_id" db:"customer_id"`
	PriceID      string      `json:"price_id" db:"price_id"`
	Quantity     float64     `json:"quantity" db:"quantity"`
	State        ReportState `json:"state" db:"state"`
	Attempts     int         `json:"attempts" db:"attempts"`
	// FirstAttemptAt is the first send: the provider's identifier window runs from there.
	FirstAttemptAt *time.Time `json:"first_attempt_at" db:"first_attempt_at"`
	LastAttemptAt  *time.Time `json:"last_attempt_at" db:"last_attempt_at"`
	// RecordedAt is the usage time sent (inside the period; inside the current invoice period when late).
	RecordedAt    *time.Time `json:"recorded_at" db:"recorded_at"`
	SentAt        *time.Time `json:"sent_at" db:"sent_at"`
	ConfirmedAt   *time.Time `json:"confirmed_at" db:"confirmed_at"`
	ProviderValue *float64   `json:"provider_value" db:"provider_value"`
	LastError     string     `json:"last_error,omitempty" db:"last_error"`
}

// Store keeps the provider's records (Postgres: migration 1500).
type Store interface {
	// Account returns org's account, or nil.
	Account(ctx context.Context, org string) (*ProviderAccount, error)
	// Accounts returns every organization's account.
	Accounts(ctx context.Context) ([]ProviderAccount, error)
	// OrganizationOf returns the organization of a provider customer, or "".
	OrganizationOf(ctx context.Context, provider, customerID string) (string, error)
	// PutAccount writes an account.
	PutAccount(ctx context.Context, a *ProviderAccount) error
	// UpdateAccount runs update on org's current account (nil when it has none) and writes what it returns (nil:
	// no change), with org's account locked against concurrent changes (webhooks, the dunning tick, operators).
	UpdateAccount(ctx context.Context, org string, update func(*ProviderAccount) (*ProviderAccount, error)) error
	// ApplyEvent claims the webhook event (provider, eventID) and, when it is new, runs update on org's locked
	// account (nil
	// when it has none; org "" for events of no organization) and writes the account it returns (nil: no change),
	// in one transaction: a failing update leaves the event unclaimed, so the provider's retry applies it again.
	// applied is false for an event received before.
	ApplyEvent(ctx context.Context, provider, eventID, eventType, org string, update func(*ProviderAccount) (*ProviderAccount, error)) (applied bool, err error)
	// Reports returns the usage reports of the period starting periodStart, or every report when it is zero.
	Reports(ctx context.Context, periodStart time.Time) ([]UsageReport, error)
	// PutReport writes a usage report. Its identifier and quantity never change once written.
	PutReport(ctx context.Context, r *UsageReport) error
	// ClaimNotice records a notification of kind for org in episode; false when it was sent before.
	ClaimNotice(ctx context.Context, org, kind, episode string) (bool, error)
	// Audit writes a billing action to the audit logs of the system tenant and of org (application
	// AuditApplication).
	Audit(ctx context.Context, by Actor, action, org string, request any) error
	// AdminEmails returns the email addresses of org's Administrators (usernames or 2FA addresses that are emails).
	AdminEmails(ctx context.Context, org string) ([]string, error)
}

// Audit actions.
const (
	AuditWebhook           = "webhook"
	AuditDunning           = "dunning"
	AuditRestore           = "restore"
	AuditConfirmSuspension = "confirm_suspension"
	AuditCheckout          = "checkout"
	AuditChangePlan        = "change_plan"
	AuditCancel            = "cancel_subscription"
)
