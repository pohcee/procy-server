package billing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrNoProvider is returned by NoProvider: billing runs without a payment provider ([billing] provider = "none").
var ErrNoProvider = errors.New("billing: no payment provider is configured")

// ErrUsageTooOld is returned by Provider.ReportUsage for usage older than the provider accepts.
var ErrUsageTooOld = errors.New("billing: the usage is older than the payment provider accepts")

// ErrSignature is wrapped by Provider.ParseWebhook when a webhook request can't be verified (missing or wrong
// signature, timestamp outside the tolerance, unreadable body). The webhook route answers 400.
var ErrSignature = errors.New("billing: webhook signature verification failed")

// Provider is a payment provider: it holds customers and subscriptions, collects money and issues invoices. Our
// usage tables are the source of truth for quantities; the provider only turns the quantities it is sent into
// invoices. Every method is safe for concurrent use.
type Provider interface {
	// Name is the provider's name ("stripe"), stored with its records.
	Name() string
	// EnsureCustomer returns the provider customer of org: customerID when it is set (checked to still exist),
	// otherwise a new customer. Never called for exempt organizations.
	EnsureCustomer(ctx context.Context, org Customer, customerID string) (string, error)
	// CheckoutURL returns a hosted page where the customer enters a payment method and starts a subscription of
	// plan.
	CheckoutURL(ctx context.Context, req CheckoutRequest) (string, error)
	// PortalURL returns the provider's customer portal for customerID (payment methods, invoices, tax ids).
	PortalURL(ctx context.Context, customerID, returnURL string) (string, error)
	// Subscription reads a subscription's current state.
	Subscription(ctx context.Context, subscriptionID string) (*Subscription, error)
	// ChangePlan moves a subscription to plan's prices.
	ChangePlan(ctx context.Context, subscriptionID string, plan *Plan) error
	// Cancel cancels a subscription, at the end of its period or at once.
	Cancel(ctx context.Context, subscriptionID string, atPeriodEnd bool) error
	// Subscriptions lists a customer's subscriptions that are not over (every status but canceled and
	// incomplete_expired).
	Subscriptions(ctx context.Context, customerID string) ([]Subscription, error)
	// ExpireOpenCheckouts expires a customer's checkout pages still open, so only the newest can be completed.
	ExpireOpenCheckouts(ctx context.Context, customerID string) error
	// InvoiceStatus returns an invoice's status (draft, open, paid, uncollectible, void).
	InvoiceStatus(ctx context.Context, invoiceID string) (string, error)
	// ReportUsage sends one usage record. It is idempotent for a short time through the record's identifier
	// (Stripe: at least 24 hours); callers check UsageSummary before sending a record again later.
	ReportUsage(ctx context.Context, rec UsageRecord) error
	// UsageSummary returns what the provider recorded for a customer and metric between from and to.
	UsageSummary(ctx context.Context, q UsageQuery) (float64, error)
	// Invoices lists a customer's most recent invoices, newest first.
	Invoices(ctx context.Context, customerID string, limit int) ([]Invoice, error)
	// ParseWebhook verifies a webhook request (the raw body and its headers) and returns the normalized event. An
	// error wrapping ErrSignature means the request is not from the provider. Events the integration doesn't
	// handle come back with Type EventIgnored.
	ParseWebhook(body []byte, header http.Header) (*Event, error)
}

// Customer is what a provider customer is created from.
type Customer struct {
	// Organization is the tenant id, stored with the customer (Stripe metadata procy_org_id).
	Organization string
	Name         string
	Email        string
}

// CheckoutRequest starts a subscription through the provider's hosted checkout.
type CheckoutRequest struct {
	Organization string
	CustomerID   string
	Plan         *Plan
	SuccessURL   string
	CancelURL    string
	// PeriodStart is the start of the next billing period (the 1st of next month, UTC): the subscription's billing
	// cycle is anchored there and the first, partial month is prorated.
	PeriodStart time.Time
	// FreeUntil, when set, is when charging starts (the end of a trial, the day after a comped date): the
	// subscription is in its trial until then, so the card is not charged before.
	FreeUntil *time.Time
}

// SubscriptionStatus is a provider subscription's state, normalized.
type SubscriptionStatus string

// Subscription states (Stripe's names).
const (
	SubscriptionIncomplete        SubscriptionStatus = "incomplete"
	SubscriptionIncompleteExpired SubscriptionStatus = "incomplete_expired"
	SubscriptionTrialing          SubscriptionStatus = "trialing"
	SubscriptionActive            SubscriptionStatus = "active"
	SubscriptionPastDue           SubscriptionStatus = "past_due"
	SubscriptionCanceled          SubscriptionStatus = "canceled"
	SubscriptionUnpaid            SubscriptionStatus = "unpaid"
	SubscriptionPaused            SubscriptionStatus = "paused"
)

// Subscription is a provider subscription.
type Subscription struct {
	ID         string             `json:"id"`
	CustomerID string             `json:"customer_id"`
	Status     SubscriptionStatus `json:"status"`
	// PriceIDs are the prices of its items.
	PriceIDs          []string `json:"price_ids"`
	CancelAtPeriodEnd bool     `json:"cancel_at_period_end"`
}

// Usage metrics reported to the provider, one meter each.
const (
	// MetricExtraDevices is the billable devices above the plan's included devices.
	MetricExtraDevices = "extra_devices"
	// MetricOverageGiB is the transfer past the allowance, in whole GiB rounded up.
	MetricOverageGiB = "overage_gib"
)

// Plan price keys of Plan.ProviderPrices.
const (
	PriceBase    = "base"
	PriceDevices = "devices"
	PriceOverage = "overage"
)

// PriceKeyOf is the Plan.ProviderPrices key of the metered price of a usage metric.
func PriceKeyOf(metric string) string {
	switch metric {
	case MetricExtraDevices:
		return PriceDevices
	case MetricOverageGiB:
		return PriceOverage
	}
	return ""
}

// UsageRecord is one quantity of one closed period, sent to the metered price PriceID of the customer.
type UsageRecord struct {
	// Identifier is deterministic (UsageIdentifier), so a resend is recognized as the same record.
	Identifier string
	CustomerID string
	// PriceID is the metered price; the provider finds its meter.
	PriceID string
	Metric  string
	// Quantity is in the metric's unit (devices, GiB); extra devices of a partial month are prorated, so it
	// may have decimals (Stripe meters accept decimal values).
	Quantity float64
	// At is the time the usage is recorded at: inside the period it belongs to.
	At time.Time
}

// UsageIdentifier is the identifier of the usage record of org's metric in the period starting periodStart:
// procy:<org>:<metric>:<YYYY-MM-DD>. Organization ids are at most 63 characters, so it stays below Stripe's 100.
func UsageIdentifier(org, metric string, periodStart time.Time) string {
	return fmt.Sprintf("procy:%s:%s:%s", org, metric, periodStart.UTC().Format("2006-01-02"))
}

// UsageQuery asks what the provider recorded for a customer on the meter of a metered price.
type UsageQuery struct {
	CustomerID string
	PriceID    string
	From, To   time.Time
}

// Invoice is a provider invoice, as listed to an organization's administrators.
type Invoice struct {
	ID     string `json:"id"`
	Number string `json:"number"`
	Status string `json:"status"`
	// Amounts in the currency's smallest unit (USD cents).
	AmountDue   int64     `json:"amount_due"`
	AmountPaid  int64     `json:"amount_paid"`
	Total       int64     `json:"total"`
	Currency    string    `json:"currency"`
	Created     time.Time `json:"created"`
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`
	// HostedURL is the provider's page for the invoice; PDF its download.
	HostedURL string `json:"hosted_url,omitempty"`
	PDF       string `json:"pdf,omitempty"`
}

// EventType is a normalized webhook event.
type EventType string

// The webhook events billing handles.
const (
	// EventCheckoutCompleted: the customer finished checkout; the subscription exists.
	EventCheckoutCompleted EventType = "checkout_completed"
	// EventSubscriptionChanged: a subscription was created or changed (status, prices).
	EventSubscriptionChanged EventType = "subscription_changed"
	// EventSubscriptionCanceled: a subscription ended.
	EventSubscriptionCanceled EventType = "subscription_canceled"
	// EventTrialWillEnd: a subscription's trial ends in a few days.
	EventTrialWillEnd EventType = "trial_will_end"
	// EventInvoicePaid: an invoice was paid; any overdue state ends.
	EventInvoicePaid EventType = "invoice_paid"
	// EventPaymentFailed: an invoice payment failed; the grace period starts.
	EventPaymentFailed EventType = "payment_failed"
	// EventIgnored: a verified event the integration doesn't handle; acknowledged and recorded.
	EventIgnored EventType = "ignored"
)

// Event is a verified webhook event, normalized.
type Event struct {
	// ID is the provider's event id, claimed once (billing_events).
	ID string
	// ProviderType is the provider's own event type ("invoice.paid").
	ProviderType string
	Type         EventType
	// Created is when the provider created the event; older events don't overwrite newer subscription state.
	Created    time.Time
	CustomerID string
	// Organization is what the provider holds as the customer's organization (metadata, client reference). It is
	// checked against our own customer mapping, never trusted alone.
	Organization       string
	SubscriptionID     string
	SubscriptionStatus SubscriptionStatus
	PriceIDs           []string
	InvoiceID          string
	// PaymentMethod is set when the event shows a payment method on file (a completed checkout, a paid invoice).
	PaymentMethod bool
}

// NoProvider is the provider of billing without payments: every call fails with ErrNoProvider.
type NoProvider struct{}

var _ Provider = NoProvider{}

// Name implements Provider.
func (NoProvider) Name() string { return "none" }

// EnsureCustomer implements Provider.
func (NoProvider) EnsureCustomer(context.Context, Customer, string) (string, error) {
	return "", ErrNoProvider
}

// CheckoutURL implements Provider.
func (NoProvider) CheckoutURL(context.Context, CheckoutRequest) (string, error) {
	return "", ErrNoProvider
}

// PortalURL implements Provider.
func (NoProvider) PortalURL(context.Context, string, string) (string, error) {
	return "", ErrNoProvider
}

// Subscription implements Provider.
func (NoProvider) Subscription(context.Context, string) (*Subscription, error) {
	return nil, ErrNoProvider
}

// ChangePlan implements Provider.
func (NoProvider) ChangePlan(context.Context, string, *Plan) error { return ErrNoProvider }

// Cancel implements Provider.
func (NoProvider) Cancel(context.Context, string, bool) error { return ErrNoProvider }

// Subscriptions implements Provider.
func (NoProvider) Subscriptions(context.Context, string) ([]Subscription, error) {
	return nil, ErrNoProvider
}

// ExpireOpenCheckouts implements Provider.
func (NoProvider) ExpireOpenCheckouts(context.Context, string) error { return ErrNoProvider }

// InvoiceStatus implements Provider.
func (NoProvider) InvoiceStatus(context.Context, string) (string, error) { return "", ErrNoProvider }

// ReportUsage implements Provider.
func (NoProvider) ReportUsage(context.Context, UsageRecord) error { return ErrNoProvider }

// UsageSummary implements Provider.
func (NoProvider) UsageSummary(context.Context, UsageQuery) (float64, error) { return 0, ErrNoProvider }

// Invoices implements Provider.
func (NoProvider) Invoices(context.Context, string, int) ([]Invoice, error) {
	return nil, ErrNoProvider
}

// ParseWebhook implements Provider.
func (NoProvider) ParseWebhook([]byte, http.Header) (*Event, error) { return nil, ErrNoProvider }
