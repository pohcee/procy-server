package stripe_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/billing"
	"github.com/pohcee/procy-server/server/billing/stripe"
	"github.com/pohcee/procy-server/server/billing/stripe/stripetest"
)

var ctx = context.Background()

var now = time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)

func newProvider(t *testing.T) (*stripe.Provider, *stripetest.Fake) {
	t.Helper()
	f := stripetest.New(t)
	f.AddLicensedPrice("price_base")
	f.AddMeteredPrice("price_devices", "procy_extra_devices")
	f.AddMeteredPrice("price_overage", "procy_overage_gib")
	p, err := stripe.New(stripe.Options{SecretKey: stripetest.SecretKey, WebhookSecret: stripetest.WebhookSecret,
		AutomaticTax: true, BackendURL: f.Server.URL, Now: func() time.Time { return now }})
	require.NoError(t, err)
	return p, f
}

var starter = &billing.Plan{ID: "starter", ProviderPrices: map[string]string{
	billing.PriceBase: "price_base", billing.PriceDevices: "price_devices", billing.PriceOverage: "price_overage"}}

func TestNewRefusesMissingSecrets(t *testing.T) {
	_, err := stripe.New(stripe.Options{WebhookSecret: "x"})
	assert.Error(t, err)
	_, err = stripe.New(stripe.Options{SecretKey: "x"})
	assert.Error(t, err)
}

func TestEnsureCustomer(t *testing.T) {
	p, f := newProvider(t)
	id, err := p.EnsureCustomer(ctx, billing.Customer{Organization: "acme", Name: "Acme", Email: "a@acme.test"}, "")
	require.NoError(t, err)
	assert.Equal(t, "acme", f.Customers[id][stripe.OrgMetadataKey])

	// an existing customer is kept
	again, err := p.EnsureCustomer(ctx, billing.Customer{Organization: "acme"}, id)
	require.NoError(t, err)
	assert.Equal(t, id, again)
	assert.Equal(t, 1, f.Count("POST /v1/customers"))

	// another organization's customer is refused
	_, err = p.EnsureCustomer(ctx, billing.Customer{Organization: "other"}, id)
	assert.Error(t, err)

	// a deleted or missing customer is replaced
	f.Deleted[id] = true
	repl, err := p.EnsureCustomer(ctx, billing.Customer{Organization: "acme"}, id)
	require.NoError(t, err)
	assert.NotEqual(t, id, repl)
	repl2, err := p.EnsureCustomer(ctx, billing.Customer{Organization: "acme"}, "cus_missing")
	require.NoError(t, err)
	assert.NotEmpty(t, repl2)
}

func TestCheckoutAnchorsTheCycleProratesAndTurnsOnTax(t *testing.T) {
	p, f := newProvider(t)
	next := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	u, err := p.CheckoutURL(ctx, billing.CheckoutRequest{Organization: "acme", CustomerID: "cus_1", Plan: starter,
		SuccessURL: "https://procy.test/billing?ok", CancelURL: "https://procy.test/billing", PeriodStart: next})
	require.NoError(t, err)
	assert.Contains(t, u, "https://checkout.stripe.test/")
	form := f.LastForm("/v1/checkout/sessions")
	assert.Equal(t, "subscription", form.Get("mode"))
	assert.Equal(t, "cus_1", form.Get("customer"))
	assert.Equal(t, "acme", form.Get("client_reference_id"))
	assert.Equal(t, "price_base", form.Get("line_items[0][price]"))
	assert.Equal(t, "1", form.Get("line_items[0][quantity]"))
	assert.Equal(t, "price_devices", form.Get("line_items[1][price]"))
	assert.Empty(t, form.Get("line_items[1][quantity]"), "metered prices have no quantity")
	assert.Equal(t, "price_overage", form.Get("line_items[2][price]"))
	assert.Equal(t, "1798761600", form.Get("subscription_data[billing_cycle_anchor]"))
	assert.Equal(t, "create_prorations", form.Get("subscription_data[proration_behavior]"))
	assert.Equal(t, "acme", form.Get("subscription_data[metadata][procy_org_id]"))
	assert.Equal(t, "true", form.Get("automatic_tax[enabled]"))
	assert.Equal(t, "auto", form.Get("customer_update[address]"))
	assert.Equal(t, "true", form.Get("tax_id_collection[enabled]"))
	assert.Equal(t, "required", form.Get("billing_address_collection"))

	_, err = p.CheckoutURL(ctx, billing.CheckoutRequest{Organization: "acme", CustomerID: "cus_1", Plan: &billing.Plan{ID: "free"}})
	assert.ErrorIs(t, err, billing.ErrConflict, "a plan without provider prices can't be bought")
}

func TestPortal(t *testing.T) {
	p, f := newProvider(t)
	u, err := p.PortalURL(ctx, "cus_1", "https://procy.test/billing")
	require.NoError(t, err)
	assert.Contains(t, u, "https://billing.stripe.test/")
	assert.Equal(t, "cus_1", f.LastForm("/v1/billing_portal/sessions").Get("customer"))
}

func TestReportUsageIsIdempotentAndFindsTheMeter(t *testing.T) {
	p, f := newProvider(t)
	at := time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC)
	rec := billing.UsageRecord{Identifier: billing.UsageIdentifier("acme", billing.MetricExtraDevices, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)),
		CustomerID: "cus_1", PriceID: "price_devices", Metric: billing.MetricExtraDevices, Quantity: 12, At: at}
	assert.Equal(t, "procy:acme:extra_devices:2026-11-01", rec.Identifier)
	require.NoError(t, p.ReportUsage(ctx, rec))
	require.NoError(t, p.ReportUsage(ctx, rec), "Stripe refusing the identifier as used is success")
	ev := f.Events()
	require.Len(t, ev, 1)
	assert.Equal(t, "procy_extra_devices", ev[0].EventName)
	assert.Equal(t, "cus_1", ev[0].Customer)
	assert.Equal(t, 12.0, ev[0].Value)
	assert.Equal(t, at, ev[0].Timestamp)
	assert.Equal(t, 1, f.Count("GET /v1/prices/price_devices"), "the meter of a price is cached")

	sum, err := p.UsageSummary(ctx, billing.UsageQuery{CustomerID: "cus_1", PriceID: "price_devices",
		From: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)})
	require.NoError(t, err)
	assert.Equal(t, 12.0, sum)

	// too old for Stripe
	old := rec
	old.Identifier = "procy:acme:extra_devices:2026-09-01"
	old.At = now.AddDate(0, 0, -40)
	assert.ErrorIs(t, p.ReportUsage(ctx, old), stripe.ErrTooOld)

	// a licensed price has no meter
	lic := rec
	lic.Identifier, lic.PriceID = "x", "price_base"
	assert.Error(t, p.ReportUsage(ctx, lic))

	// errors come back
	f.FailMeterEvents = true
	rec.Identifier = "procy:acme:overage_gib:2026-11-01"
	rec.PriceID = "price_overage"
	assert.Error(t, p.ReportUsage(ctx, rec))
}

func TestSubscriptionChangeAndCancel(t *testing.T) {
	p, f := newProvider(t)
	f.Subscriptions["sub_1"] = &stripetest.Subscription{ID: "sub_1", Customer: "cus_1", Status: "active", Items: []string{"price_old"}}
	s, err := p.Subscription(ctx, "sub_1")
	require.NoError(t, err)
	assert.Equal(t, billing.SubscriptionActive, s.Status)
	assert.Equal(t, "cus_1", s.CustomerID)
	assert.Equal(t, []string{"price_old"}, s.PriceIDs)

	require.NoError(t, p.ChangePlan(ctx, "sub_1", starter))
	assert.Equal(t, []string{"price_base", "price_devices", "price_overage"}, f.Subscriptions["sub_1"].Items)

	require.NoError(t, p.Cancel(ctx, "sub_1", true))
	assert.True(t, f.Subscriptions["sub_1"].Cancel)
	require.NoError(t, p.Cancel(ctx, "sub_1", false))
	assert.Equal(t, "canceled", f.Subscriptions["sub_1"].Status)

	_, err = p.Subscription(ctx, "sub_missing")
	assert.ErrorIs(t, err, billing.ErrNotFound)
}

func TestInvoices(t *testing.T) {
	p, f := newProvider(t)
	f.Invoices["cus_1"] = []map[string]any{
		{"id": "in_2", "object": "invoice", "status": "paid", "number": "A-2", "total": 2725, "amount_paid": 2725, "currency": "usd", "created": now.Unix(), "hosted_invoice_url": "https://invoice.stripe.test/2"},
		{"id": "in_draft", "object": "invoice", "status": "draft"},
		{"id": "in_1", "object": "invoice", "status": "open", "number": "A-1", "total": 1000, "amount_due": 1000, "currency": "usd"},
	}
	inv, err := p.Invoices(ctx, "cus_1", 10)
	require.NoError(t, err)
	require.Len(t, inv, 2, "drafts are not listed")
	assert.Equal(t, "A-2", inv[0].Number)
	assert.Equal(t, int64(2725), inv[0].Total)
	assert.Equal(t, "https://invoice.stripe.test/2", inv[0].HostedURL)
}

func header(sig string) http.Header {
	h := http.Header{}
	h.Set("Stripe-Signature", sig)
	return h
}

func TestWebhookSignature(t *testing.T) {
	p, _ := newProvider(t)
	inv := map[string]any{"id": "in_1", "object": "invoice", "customer": "cus_1", "amount_paid": 2725,
		"parent": map[string]any{"type": "subscription_details", "subscription_details": map[string]any{"subscription": "sub_1", "metadata": map[string]any{"procy_org_id": "acme"}}}}
	body, sig := stripetest.SignedEvent("evt_1", "invoice.paid", inv, stripetest.WebhookSecret, time.Now())

	e, err := p.ParseWebhook(body, header(sig))
	require.NoError(t, err)
	assert.Equal(t, billing.EventInvoicePaid, e.Type)
	assert.Equal(t, "evt_1", e.ID)
	assert.Equal(t, "cus_1", e.CustomerID)
	assert.Equal(t, "sub_1", e.SubscriptionID)
	assert.Equal(t, "in_1", e.InvoiceID)
	assert.Equal(t, "acme", e.Organization)
	assert.True(t, e.PaymentMethod)

	for name, tc := range map[string]struct {
		body []byte
		sig  string
	}{
		"no signature":   {body, ""},
		"tampered body":  {append(append([]byte(nil), body[:len(body)-1]...), ' ', '}'), sig},
		"garbage header": {body, "nonsense"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.ParseWebhook(tc.body, header(tc.sig))
			assert.ErrorIs(t, err, billing.ErrSignature)
		})
	}

	t.Run("replayed after the tolerance", func(t *testing.T) {
		old, oldSig := stripetest.SignedEvent("evt_1", "invoice.paid", inv, stripetest.WebhookSecret, time.Now().Add(-6*time.Minute))
		_, err := p.ParseWebhook(old, header(oldSig))
		assert.ErrorIs(t, err, billing.ErrSignature)
	})
	t.Run("wrong secret signature", func(t *testing.T) {
		b, s := stripetest.SignedEvent("evt_1", "invoice.paid", inv, "whsec_other", time.Now())
		_, err := p.ParseWebhook(b, header(s))
		assert.ErrorIs(t, err, billing.ErrSignature)
	})
}

func TestWebhookNormalizesEvents(t *testing.T) {
	p, f := newProvider(t)
	parse := func(typ string, obj any) *billing.Event {
		t.Helper()
		b, s := stripetest.SignedEvent("evt_"+typ, typ, obj, stripetest.WebhookSecret, time.Now())
		e, err := p.ParseWebhook(b, header(s))
		require.NoError(t, err)
		return e
	}
	sub := f.SubscriptionJSON(&stripetest.Subscription{ID: "sub_1", Customer: "cus_1", Status: "past_due", Items: []string{"price_base"}, Metadata: map[string]string{"procy_org_id": "acme"}})

	e := parse("customer.subscription.updated", sub)
	assert.Equal(t, billing.EventSubscriptionChanged, e.Type)
	assert.Equal(t, billing.SubscriptionPastDue, e.SubscriptionStatus)
	assert.Equal(t, []string{"price_base"}, e.PriceIDs)
	assert.Equal(t, "acme", e.Organization)
	assert.Equal(t, billing.EventSubscriptionChanged, parse("customer.subscription.created", sub).Type)
	assert.Equal(t, billing.EventSubscriptionCanceled, parse("customer.subscription.deleted", sub).Type)
	assert.Equal(t, billing.EventTrialWillEnd, parse("customer.subscription.trial_will_end", sub).Type)

	e = parse("checkout.session.completed", map[string]any{"id": "cs_1", "object": "checkout.session", "mode": "subscription",
		"status": "complete", "customer": "cus_1", "subscription": "sub_1", "client_reference_id": "acme"})
	assert.Equal(t, billing.EventCheckoutCompleted, e.Type)
	assert.Equal(t, "sub_1", e.SubscriptionID)
	assert.Equal(t, "acme", e.Organization)
	assert.True(t, e.PaymentMethod)

	e = parse("invoice.payment_failed", map[string]any{"id": "in_1", "object": "invoice", "customer": "cus_1"})
	assert.Equal(t, billing.EventPaymentFailed, e.Type)
	assert.False(t, e.PaymentMethod)

	assert.Equal(t, billing.EventIgnored, parse("customer.created", map[string]any{"id": "cus_1", "object": "customer"}).Type)
	assert.Equal(t, billing.EventIgnored, parse("checkout.session.completed", map[string]any{"id": "cs_2", "object": "checkout.session", "mode": "payment"}).Type)
}

func TestWebhookOfAnotherReleaseTrainIsNotASignatureError(t *testing.T) {
	p, _ := newProvider(t)
	// a correctly signed event of another API version: not forged, but not readable either
	body := []byte(`{"id":"evt_x","object":"event","type":"invoice.paid","api_version":"2024-06-20","created":1,"data":{"object":{"id":"in_1","object":"invoice"}}}`)
	sig := stripetest.SignHeader(body, stripetest.WebhookSecret, time.Now())
	_, err := p.ParseWebhook(body, header(sig))
	require.Error(t, err)
	assert.False(t, errors.Is(err, billing.ErrSignature))
}
