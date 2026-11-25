// Package stripe is the Stripe billing.Provider (billing and metering design, phase 4), on the official stripe-go
// client. Products, prices and meters are created in Stripe by the operators (from their private configuration);
// rportd only reads them: a plan's provider prices name the price ids, and each metered price's meter is found
// from the price.
//
// Stripe details confirmed against docs.stripe.com on 2026-11-24 (stripe-go v87.1.0, API version 2026-10-28.endive):
//   - Meter events (POST /v1/billing/meter_events): identifier at most 100 characters, unique only within a
//     rolling period of at least 24 hours; timestamp within the past 35 days and at most 5 minutes ahead; payload
//     keys are the meter's customer_mapping and value_settings keys.
//   - Usage counts on a subscription cycle invoice only if it arrives before the invoice finalizes: by default an
//     hour after the period ends, configurable up to 72 hours (Invoice settings, "finalization grace period").
//   - Meter event summaries: start and end times aligned to minutes.
//   - Webhooks: Stripe-Signature "t=…,v1=…", HMAC-SHA256 over "t.body" with the endpoint secret, 5 minutes'
//     tolerance; events may arrive twice and out of order; Stripe retries failed deliveries for up to 3 days.
//     stripe-go refuses events of another API release train than its own ("endive").
package stripe

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	sg "github.com/stripe/stripe-go/v87"

	"github.com/openrport/openrport/server/billing"
)

// OrgMetadataKey is the metadata key holding the organization id on Stripe customers, subscriptions and checkout
// sessions.
const OrgMetadataKey = "procy_org_id"

// planMetadataKey is the metadata key holding the plan id on subscriptions and checkout sessions.
const planMetadataKey = "procy_plan"

// WebhookTolerance is how old a webhook's signed timestamp may be.
const WebhookTolerance = 5 * time.Minute

// Options configure the provider.
type Options struct {
	// SecretKey is the Stripe secret (or restricted) API key.
	SecretKey string
	// WebhookSecret is the signing secret of the webhook endpoint ("whsec_…").
	WebhookSecret string
	// AutomaticTax turns on Stripe Tax in checkout (sales tax for the US and Canada, decision 12; the
	// registrations are Stripe configuration).
	AutomaticTax bool
	// BackendURL replaces https://api.stripe.com (tests).
	BackendURL string
	// HTTPClient replaces the default client (tests).
	HTTPClient *http.Client
	// Now replaces time.Now (tests).
	Now func() time.Time
}

// Provider is the Stripe billing.Provider.
type Provider struct {
	sc            *sg.Client
	webhookSecret string
	automaticTax  bool
	now           func() time.Time

	mu sync.Mutex
	// meters caches the meter of each metered price: prices and meters can't change once used
	meters map[string]*meter
}

type meter struct {
	id, eventName, customerKey, valueKey string
}

var _ billing.Provider = (*Provider)(nil)

// New returns the provider.
func New(o Options) (*Provider, error) {
	if o.SecretKey == "" {
		return nil, errors.New("stripe: the secret key is empty")
	}
	if o.WebhookSecret == "" {
		return nil, errors.New("stripe: the webhook secret is empty")
	}
	cfg := &sg.BackendConfig{
		HTTPClient:        o.HTTPClient,
		MaxNetworkRetries: sg.Int64(2),
		// stripe-go's default logger writes to stderr; errors come back to the callers, which log them
		LeveledLogger: &sg.LeveledLogger{Level: sg.LevelNull},
	}
	if o.BackendURL != "" {
		cfg.URL = sg.String(o.BackendURL)
	}
	backends := &sg.Backends{
		API:         sg.GetBackendWithConfig(sg.APIBackend, cfg),
		Connect:     sg.GetBackendWithConfig(sg.ConnectBackend, cfg),
		Uploads:     sg.GetBackendWithConfig(sg.UploadsBackend, cfg),
		MeterEvents: sg.GetBackendWithConfig(sg.MeterEventsBackend, cfg),
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Provider{
		sc:            sg.NewClient(o.SecretKey, sg.WithBackends(backends)),
		webhookSecret: o.WebhookSecret,
		automaticTax:  o.AutomaticTax,
		now:           now,
		meters:        map[string]*meter{},
	}, nil
}

// Name implements billing.Provider.
func (p *Provider) Name() string { return "stripe" }

// EnsureCustomer implements billing.Provider: the customer customerID if it still exists (and isn't deleted) and
// belongs to org, otherwise a new customer with org in its metadata. The customer is created with an idempotency key
// derived from org, so two api processes creating it at once get the same one.
func (p *Provider) EnsureCustomer(ctx context.Context, org billing.Customer, customerID string) (string, error) {
	if org.Organization == "" {
		return "", errors.New("stripe: customer without organization")
	}
	if customerID != "" {
		c, err := p.sc.V1Customers.Retrieve(ctx, customerID, nil)
		switch {
		case err == nil && !c.Deleted:
			if got := c.Metadata[OrgMetadataKey]; got != "" && got != org.Organization {
				return "", fmt.Errorf("stripe: customer %s belongs to organization %q, not %q", customerID, got, org.Organization)
			}
			return c.ID, nil
		case err != nil && !isNotFound(err):
			return "", fmt.Errorf("stripe: retrieve customer: %w", err)
		}
	}
	params := &sg.CustomerCreateParams{
		Metadata: map[string]string{OrgMetadataKey: org.Organization},
	}
	if org.Name != "" {
		params.Name = sg.String(org.Name)
	}
	if org.Email != "" {
		params.Email = sg.String(org.Email)
	}
	// a replaced (deleted) customer gets a new key: Stripe keeps idempotency keys for 24 hours
	params.SetIdempotencyKey("procy-customer-" + org.Organization + "-" + customerID)
	c, err := p.sc.V1Customers.Create(ctx, params)
	if err != nil {
		return "", fmt.Errorf("stripe: create customer: %w", err)
	}
	return c.ID, nil
}

// lineItems are the subscription's prices: the base price with quantity 1, the metered prices without quantity.
func lineItems(plan *billing.Plan) ([]string, []string, error) {
	if plan == nil {
		return nil, nil, fmt.Errorf("%w: no plan", billing.ErrConflict)
	}
	var licensed, metered []string
	if id := plan.ProviderPrices[billing.PriceBase]; id != "" {
		licensed = append(licensed, id)
	}
	for _, k := range []string{billing.PriceDevices, billing.PriceOverage} {
		if id := plan.ProviderPrices[k]; id != "" {
			metered = append(metered, id)
		}
	}
	if len(licensed)+len(metered) == 0 {
		return nil, nil, fmt.Errorf("%w: plan %q has no provider prices", billing.ErrConflict, plan.ID)
	}
	return licensed, metered, nil
}

// CheckoutURL implements billing.Provider: a Checkout Session in subscription mode for the existing customer, its
// billing cycle anchored at req.PeriodStart (the 1st of a month, UTC) with the partial first month prorated, Stripe
// Tax on when configured (the billing address is collected and saved on the customer, tax ids too).
func (p *Provider) CheckoutURL(ctx context.Context, req billing.CheckoutRequest) (string, error) {
	licensed, metered, err := lineItems(req.Plan)
	if err != nil {
		return "", err
	}
	meta := map[string]string{OrgMetadataKey: req.Organization, planMetadataKey: req.Plan.ID}
	params := &sg.CheckoutSessionCreateParams{
		Mode:              sg.String(string(sg.CheckoutSessionModeSubscription)),
		Customer:          sg.String(req.CustomerID),
		ClientReferenceID: sg.String(req.Organization),
		SuccessURL:        sg.String(req.SuccessURL),
		CancelURL:         sg.String(req.CancelURL),
		Metadata:          meta,
		SubscriptionData: &sg.CheckoutSessionCreateSubscriptionDataParams{
			Metadata:          meta,
			ProrationBehavior: sg.String("create_prorations"),
		},
	}
	if !req.PeriodStart.IsZero() && req.PeriodStart.After(p.now()) {
		params.SubscriptionData.BillingCycleAnchor = sg.Int64(req.PeriodStart.Unix())
	}
	for _, id := range licensed {
		params.LineItems = append(params.LineItems, &sg.CheckoutSessionCreateLineItemParams{Price: sg.String(id), Quantity: sg.Int64(1)})
	}
	for _, id := range metered {
		params.LineItems = append(params.LineItems, &sg.CheckoutSessionCreateLineItemParams{Price: sg.String(id)})
	}
	if p.automaticTax {
		params.AutomaticTax = &sg.CheckoutSessionCreateAutomaticTaxParams{Enabled: sg.Bool(true)}
		params.BillingAddressCollection = sg.String("required")
		params.CustomerUpdate = &sg.CheckoutSessionCreateCustomerUpdateParams{Address: sg.String("auto"), Name: sg.String("auto")}
		params.TaxIDCollection = &sg.CheckoutSessionCreateTaxIDCollectionParams{Enabled: sg.Bool(true)}
	}
	s, err := p.sc.V1CheckoutSessions.Create(ctx, params)
	if err != nil {
		return "", fmt.Errorf("stripe: create checkout session: %w", err)
	}
	return s.URL, nil
}

// PortalURL implements billing.Provider.
func (p *Provider) PortalURL(ctx context.Context, customerID, returnURL string) (string, error) {
	s, err := p.sc.V1BillingPortalSessions.Create(ctx, &sg.BillingPortalSessionCreateParams{
		Customer:  sg.String(customerID),
		ReturnURL: sg.String(returnURL),
	})
	if err != nil {
		return "", fmt.Errorf("stripe: create portal session: %w", err)
	}
	return s.URL, nil
}

// Subscription implements billing.Provider.
func (p *Provider) Subscription(ctx context.Context, subscriptionID string) (*billing.Subscription, error) {
	s, err := p.sc.V1Subscriptions.Retrieve(ctx, subscriptionID, nil)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("stripe: subscription %s: %w", subscriptionID, billing.ErrNotFound)
		}
		return nil, fmt.Errorf("stripe: retrieve subscription: %w", err)
	}
	return subscriptionOf(s), nil
}

func subscriptionOf(s *sg.Subscription) *billing.Subscription {
	out := &billing.Subscription{
		ID:                s.ID,
		Status:            billing.SubscriptionStatus(s.Status),
		CancelAtPeriodEnd: s.CancelAtPeriodEnd,
		PriceIDs:          priceIDs(s),
	}
	if s.Customer != nil {
		out.CustomerID = s.Customer.ID
	}
	return out
}

func priceIDs(s *sg.Subscription) []string {
	var ids []string
	if s.Items != nil {
		for _, it := range s.Items.Data {
			if it != nil && it.Price != nil {
				ids = append(ids, it.Price.ID)
			}
		}
	}
	return ids
}

// ChangePlan implements billing.Provider: the subscription's items are replaced by the plan's prices, prorated.
// Stripe drops usage reported in the invoice finalization grace period for a cycle in which a price changed, so the
// usage of the previous period must be reported before a plan change (the reporting job runs right after the
// period closes).
func (p *Provider) ChangePlan(ctx context.Context, subscriptionID string, plan *billing.Plan) error {
	licensed, metered, err := lineItems(plan)
	if err != nil {
		return err
	}
	s, err := p.sc.V1Subscriptions.Retrieve(ctx, subscriptionID, nil)
	if err != nil {
		return fmt.Errorf("stripe: retrieve subscription: %w", err)
	}
	params := &sg.SubscriptionUpdateParams{
		ProrationBehavior: sg.String("create_prorations"),
		Metadata:          map[string]string{planMetadataKey: plan.ID},
	}
	if s.Items != nil {
		for _, it := range s.Items.Data {
			params.Items = append(params.Items, &sg.SubscriptionUpdateItemParams{ID: sg.String(it.ID), Deleted: sg.Bool(true)})
		}
	}
	for _, id := range licensed {
		params.Items = append(params.Items, &sg.SubscriptionUpdateItemParams{Price: sg.String(id), Quantity: sg.Int64(1)})
	}
	for _, id := range metered {
		params.Items = append(params.Items, &sg.SubscriptionUpdateItemParams{Price: sg.String(id)})
	}
	if _, err := p.sc.V1Subscriptions.Update(ctx, subscriptionID, params); err != nil {
		return fmt.Errorf("stripe: change plan: %w", err)
	}
	return nil
}

// Cancel implements billing.Provider.
func (p *Provider) Cancel(ctx context.Context, subscriptionID string, atPeriodEnd bool) error {
	if atPeriodEnd {
		_, err := p.sc.V1Subscriptions.Update(ctx, subscriptionID, &sg.SubscriptionUpdateParams{CancelAtPeriodEnd: sg.Bool(true)})
		if err != nil {
			return fmt.Errorf("stripe: cancel at period end: %w", err)
		}
		return nil
	}
	if _, err := p.sc.V1Subscriptions.Cancel(ctx, subscriptionID, nil); err != nil {
		return fmt.Errorf("stripe: cancel: %w", err)
	}
	return nil
}

// meterOf returns the meter of a metered price (cached).
func (p *Provider) meterOf(ctx context.Context, priceID string) (*meter, error) {
	p.mu.Lock()
	m := p.meters[priceID]
	p.mu.Unlock()
	if m != nil {
		return m, nil
	}
	price, err := p.sc.V1Prices.Retrieve(ctx, priceID, nil)
	if err != nil {
		return nil, fmt.Errorf("stripe: retrieve price %s: %w", priceID, err)
	}
	if price.Recurring == nil || price.Recurring.Meter == "" {
		return nil, fmt.Errorf("stripe: price %s is not a metered price backed by a billing meter", priceID)
	}
	bm, err := p.sc.V1BillingMeters.Retrieve(ctx, price.Recurring.Meter, nil)
	if err != nil {
		return nil, fmt.Errorf("stripe: retrieve meter %s: %w", price.Recurring.Meter, err)
	}
	m = &meter{id: bm.ID, eventName: bm.EventName, customerKey: "stripe_customer_id", valueKey: "value"}
	if bm.CustomerMapping != nil && bm.CustomerMapping.EventPayloadKey != "" {
		m.customerKey = bm.CustomerMapping.EventPayloadKey
	}
	if bm.ValueSettings != nil && bm.ValueSettings.EventPayloadKey != "" {
		m.valueKey = bm.ValueSettings.EventPayloadKey
	}
	p.mu.Lock()
	p.meters[priceID] = m
	p.mu.Unlock()
	return m, nil
}

// maxEventAge is how old a meter event's timestamp may be (Stripe: 35 calendar days), with a day of margin.
const maxEventAge = 34 * 24 * time.Hour

// ErrTooOld is returned by ReportUsage for usage older than Stripe accepts.
var ErrTooOld = errors.New("stripe: usage is older than Stripe accepts (35 days)")

// ReportUsage implements billing.Provider: one meter event with the record's identifier. Stripe answering that the
// identifier was used already counts as success (the record was sent before).
func (p *Provider) ReportUsage(ctx context.Context, rec billing.UsageRecord) error {
	if rec.Identifier == "" || len(rec.Identifier) > 100 {
		return fmt.Errorf("stripe: meter event identifier must have 1 to 100 characters: %q", rec.Identifier)
	}
	if rec.Quantity < 0 {
		return fmt.Errorf("stripe: negative usage %d", rec.Quantity)
	}
	if p.now().Sub(rec.At) > maxEventAge {
		return ErrTooOld
	}
	m, err := p.meterOf(ctx, rec.PriceID)
	if err != nil {
		return err
	}
	params := &sg.BillingMeterEventCreateParams{
		EventName:  sg.String(m.eventName),
		Identifier: sg.String(rec.Identifier),
		Timestamp:  sg.Int64(rec.At.Unix()),
		Payload: map[string]string{
			m.customerKey: rec.CustomerID,
			m.valueKey:    strconv.FormatInt(rec.Quantity, 10),
		},
	}
	params.SetIdempotencyKey(rec.Identifier)
	if _, err := p.sc.V1BillingMeterEvents.Create(ctx, params); err != nil {
		if isDuplicateIdentifier(err) {
			return nil
		}
		return fmt.Errorf("stripe: meter event %s: %w", rec.Identifier, err)
	}
	return nil
}

// UsageSummary implements billing.Provider: the sum of the meter's event summaries for the customer between q.From
// and q.To (truncated to minutes, as Stripe requires).
func (p *Provider) UsageSummary(ctx context.Context, q billing.UsageQuery) (float64, error) {
	m, err := p.meterOf(ctx, q.PriceID)
	if err != nil {
		return 0, err
	}
	params := &sg.BillingMeterEventSummaryListParams{
		ID:        sg.String(m.id),
		Customer:  sg.String(q.CustomerID),
		StartTime: sg.Int64(q.From.Truncate(time.Minute).Unix()),
		EndTime:   sg.Int64(q.To.Truncate(time.Minute).Unix()),
	}
	var sum float64
	for s, err := range p.sc.V1BillingMeterEventSummaries.List(ctx, params).All(ctx) {
		if err != nil {
			return 0, fmt.Errorf("stripe: meter event summaries: %w", err)
		}
		sum += s.AggregatedValue
	}
	return sum, nil
}

// Invoices implements billing.Provider.
func (p *Provider) Invoices(ctx context.Context, customerID string, limit int) ([]billing.Invoice, error) {
	limit = max(1, min(limit, 100))
	params := &sg.InvoiceListParams{Customer: sg.String(customerID)}
	params.Limit = sg.Int64(int64(limit))
	out := []billing.Invoice{}
	for inv, err := range p.sc.V1Invoices.List(ctx, params).All(ctx) {
		if err != nil {
			return nil, fmt.Errorf("stripe: list invoices: %w", err)
		}
		if inv.Status == sg.InvoiceStatusDraft {
			continue
		}
		out = append(out, billing.Invoice{
			ID:          inv.ID,
			Number:      inv.Number,
			Status:      string(inv.Status),
			AmountDue:   inv.AmountDue,
			AmountPaid:  inv.AmountPaid,
			Total:       inv.Total,
			Currency:    string(inv.Currency),
			Created:     time.Unix(inv.Created, 0).UTC(),
			PeriodStart: time.Unix(inv.PeriodStart, 0).UTC(),
			PeriodEnd:   time.Unix(inv.PeriodEnd, 0).UTC(),
			HostedURL:   inv.HostedInvoiceURL,
			PDF:         inv.InvoicePDF,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func isNotFound(err error) bool {
	var se *sg.Error
	return errors.As(err, &se) && (se.HTTPStatusCode == http.StatusNotFound || se.Code == sg.ErrorCodeResourceMissing)
}

// isDuplicateIdentifier reports whether Stripe refused a meter event because its identifier was used already.
func isDuplicateIdentifier(err error) bool {
	var se *sg.Error
	if !errors.As(err, &se) {
		return false
	}
	if se.Code == sg.ErrorCodeResourceAlreadyExists {
		return true
	}
	msg := strings.ToLower(se.Msg)
	return strings.Contains(msg, "identifier") && (strings.Contains(msg, "already") || strings.Contains(msg, "duplicate"))
}
