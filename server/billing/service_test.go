package billing_test

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/billing"
	"github.com/openrport/openrport/server/billing/billingtest"
	"github.com/openrport/openrport/server/billing/stripe"
	"github.com/openrport/openrport/server/billing/stripe/stripetest"
	"github.com/openrport/openrport/share/tenant"
)

var ctx = context.Background()

type notice struct{ org, subject string }

type recNotifier struct {
	mu   sync.Mutex
	sent []notice
}

func (n *recNotifier) Notify(_ context.Context, org tenant.ID, _ []string, subject, _ string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, notice{string(org), subject})
	return nil
}

func (n *recNotifier) subjects() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, s := range n.sent {
		out = append(out, s.subject)
	}
	return out
}

type recSuspender struct{ suspended []string }

func (r *recSuspender) Suspend(_ context.Context, _ billing.Actor, org string) error {
	r.suspended = append(r.suspended, org)
	return nil
}

type records map[string]*billing.Billing

func (r records) Billing(_ context.Context, org string) (*billing.Billing, error) { return r[org], nil }

type periods []billing.Period

func (p periods) ClosedSince(context.Context, time.Time) ([]billing.Period, error) { return p, nil }

type plans map[string]*billing.Plan

func (p plans) Get(id string) *billing.Plan { return p[id] }

type syncRec struct {
	mu       sync.Mutex
	reported []string
	states   []string
}

func (s *syncRec) ProviderState(_ context.Context, org, _, sub string, pm bool, plan string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := org + " " + sub + " plan=" + plan
	if pm {
		st += " pm"
	}
	s.states = append(s.states, st)
	return nil
}

func (s *syncRec) PeriodReported(_ context.Context, org string, start, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reported = append(s.reported, org+" "+start.Format("2006-01"))
	return nil
}

type env struct {
	svc     *billing.Service
	store   *billingtest.MemStore
	fake    *stripetest.Fake
	notes   *recNotifier
	susp    *recSuspender
	sync    *syncRec
	records records
	periods *periods
	now     *time.Time
}

var starter = &billing.Plan{ID: "starter", IncludedDevices: 10, Overage: billing.OverageBill, OveragePricePerGiB: 0.25,
	ProviderPrices: map[string]string{billing.PriceBase: "price_base", billing.PriceDevices: "price_devices", billing.PriceOverage: "price_overage"}}
var team = &billing.Plan{ID: "team", ProviderPrices: map[string]string{billing.PriceBase: "price_team", billing.PriceDevices: "price_devices"}}
var free = &billing.Plan{ID: "free"}

func newEnv(t *testing.T) *env {
	t.Helper()
	f := stripetest.New(t)
	f.AddLicensedPrice("price_base")
	f.AddLicensedPrice("price_team")
	f.AddMeteredPrice("price_devices", "procy_extra_devices")
	f.AddMeteredPrice("price_overage", "procy_overage_gib")
	now := time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)
	e := &env{fake: f, store: billingtest.NewMemStore(), notes: &recNotifier{}, susp: &recSuspender{}, sync: &syncRec{},
		records: records{}, periods: &periods{}, now: &now}
	p, err := stripe.New(stripe.Options{SecretKey: stripetest.SecretKey, WebhookSecret: stripetest.WebhookSecret,
		BackendURL: f.Server.URL, Now: func() time.Time { return *e.now }})
	require.NoError(t, err)
	e.svc, err = billing.NewService(billing.Options{
		Provider: p, Store: e.store, Plans: plans{"starter": starter, "team": team, "free": free}, PlanIDs: []string{"free", "starter", "team"},
		FreePlan: "free", Records: e.records,
		Periods: periodsRef{e.periods}, Sync: e.sync, Notifier: e.notes, Suspender: e.susp, ReturnURL: "https://procy.test/billing",
		Now: func() time.Time { return *e.now },
	})
	require.NoError(t, err)
	return e
}

type periodsRef struct{ p *periods }

func (r periodsRef) ClosedSince(ctx context.Context, since time.Time) ([]billing.Period, error) {
	return r.p.ClosedSince(ctx, since)
}

func (e *env) link(org, customer string) {
	e.store.Accts[org] = billing.ProviderAccount{Organization: org, Provider: "stripe", CustomerID: customer, SubscriptionID: "sub_" + org,
		SubscriptionStatus: billing.SubscriptionActive, Dunning: billing.Dunning{State: billing.DunningNone}}
	e.store.Emails[org] = []string{"admin@" + org + ".test"}
}

func (e *env) webhook(t *testing.T, id, typ string, obj any) billing.WebhookResult {
	t.Helper()
	body, sig := stripetest.SignedEvent(id, typ, obj, stripetest.WebhookSecret, time.Now())
	h := http.Header{}
	h.Set("Stripe-Signature", sig)
	res, err := e.svc.HandleWebhook(ctx, body, h)
	if res != billing.WebhookUnverified {
		require.NoError(t, err)
	}
	return res
}

func invoice(id, customer string) map[string]any {
	return map[string]any{"id": id, "object": "invoice", "customer": customer, "amount_paid": 100}
}

func TestWebhookIsVerifiedAndAppliedOnce(t *testing.T) {
	e := newEnv(t)
	e.link("acme", "cus_1")

	assert.Equal(t, billing.WebhookApplied, e.webhook(t, "evt_1", "invoice.payment_failed", invoice("in_1", "cus_1")))
	assert.Equal(t, billing.WebhookDuplicate, e.webhook(t, "evt_1", "invoice.payment_failed", invoice("in_1", "cus_1")), "a replayed event changes nothing")

	body, _ := stripetest.SignedEvent("evt_2", "invoice.paid", invoice("in_1", "cus_1"), "whsec_attacker", time.Now())
	h := http.Header{}
	h.Set("Stripe-Signature", stripetest.SignHeader(body, "whsec_attacker", time.Now()))
	res, err := e.svc.HandleWebhook(ctx, body, h)
	assert.Equal(t, billing.WebhookUnverified, res)
	assert.ErrorIs(t, err, billing.ErrSignature)
	a, _ := e.store.Account(ctx, "acme")
	assert.Equal(t, billing.DunningPastDue, a.State, "the forged payment did nothing")

	st := e.svc.Stats()
	assert.Equal(t, int64(1), st.WebhooksAccepted)
	assert.Equal(t, int64(1), st.WebhooksDuplicate)
	assert.Equal(t, int64(1), st.WebhooksUnverified)
	assert.Contains(t, e.store.Audits, "billing:stripe webhook acme")
}

func TestWebhookTrustsOurCustomerMappingOnly(t *testing.T) {
	e := newEnv(t)
	e.link("acme", "cus_1")
	e.link("other", "cus_2")
	sub := e.fake.SubscriptionJSON(&stripetest.Subscription{ID: "sub_x", Customer: "cus_2", Status: "canceled", Metadata: map[string]string{"procy_org_id": "acme"}})
	assert.Equal(t, billing.WebhookApplied, e.webhook(t, "evt_1", "customer.subscription.deleted", sub))
	a, _ := e.store.Account(ctx, "other")
	assert.Equal(t, billing.SubscriptionActive, a.SubscriptionStatus, "metadata naming another organization is ignored")
	a, _ = e.store.Account(ctx, "acme")
	assert.Equal(t, billing.SubscriptionActive, a.SubscriptionStatus)

	// an unknown customer is acknowledged and ignored
	assert.Equal(t, billing.WebhookApplied, e.webhook(t, "evt_2", "invoice.payment_failed", invoice("in_9", "cus_unknown")))
}

func TestSubscriptionEventsOutOfOrder(t *testing.T) {
	e := newEnv(t)
	e.link("acme", "cus_1")
	newer := e.fake.SubscriptionJSON(&stripetest.Subscription{ID: "sub_acme", Customer: "cus_1", Status: "active", Items: []string{"price_team"}})
	older := e.fake.SubscriptionJSON(&stripetest.Subscription{ID: "sub_acme", Customer: "cus_1", Status: "incomplete", Items: []string{"price_base"}})
	t0 := time.Now()
	apply := func(id string, obj any, at time.Time) {
		body, sig := stripetest.SignedEvent(id, "customer.subscription.updated", obj, stripetest.WebhookSecret, t0)
		// the event's own creation time decides the order
		body = []byte(replaceCreated(string(body), at.Unix()))
		h := http.Header{}
		h.Set("Stripe-Signature", stripetest.SignHeader(body, stripetest.WebhookSecret, t0))
		_ = sig
		_, err := e.svc.HandleWebhook(ctx, body, h)
		require.NoError(t, err)
	}
	apply("evt_new", newer, t0)
	apply("evt_old", older, t0.Add(-time.Minute))
	a, _ := e.store.Account(ctx, "acme")
	assert.Equal(t, billing.SubscriptionActive, a.SubscriptionStatus)
	assert.Equal(t, []string{"price_team"}, a.PriceIDs)
	assert.Contains(t, e.sync.states, "acme sub_acme plan=team", "the plan bought is passed on")
}

func TestDunningStateMachine(t *testing.T) {
	e := newEnv(t)
	e.link("acme", "cus_1")
	failedAt := time.Now().UTC().Truncate(time.Second)
	e.webhook(t, "evt_f1", "invoice.payment_failed", invoice("in_1", "cus_1"))
	a, _ := e.store.Account(ctx, "acme")
	require.Equal(t, billing.DunningPastDue, a.State)
	assert.Equal(t, billing.AccessFull, e.svc.Access(ctx, "acme"), "full service during the grace period")
	assert.Equal(t, []string{"Procy billing: Payment failed"}, e.notes.subjects())

	// a retry failing again keeps the episode's first failure
	e.webhook(t, "evt_f2", "invoice.payment_failed", invoice("in_1", "cus_1"))
	a, _ = e.store.Account(ctx, "acme")
	assert.WithinDuration(t, failedAt, *a.FailedAt, 5*time.Second)
	assert.Len(t, e.notes.subjects(), 1, "one notification per episode")

	// 13 days: still past due
	*e.now = a.FailedAt.Add(13 * 24 * time.Hour)
	require.NoError(t, e.svc.DunningTick(ctx))
	a, _ = e.store.Account(ctx, "acme")
	assert.Equal(t, billing.DunningPastDue, a.State)
	assert.Error(t, e.svc.ConfirmSuspension(ctx, billing.Actor{Username: "op"}, "acme"), "suspension is not proposed yet")

	// 14 days: read-only
	*e.now = a.FailedAt.Add(14 * 24 * time.Hour)
	require.NoError(t, e.svc.DunningTick(ctx))
	a, _ = e.store.Account(ctx, "acme")
	assert.Equal(t, billing.DunningReadOnly, a.State)
	assert.Equal(t, billing.AccessReadOnly, e.svc.Access(ctx, "acme"))
	_, err := e.svc.Checkout(ctx, billing.Actor{Username: "admin@acme.test"}, "acme", "Acme", "team")
	assert.ErrorIs(t, err, billing.ErrReadOnly)
	_, err = e.svc.PortalURL(ctx, billing.Actor{Username: "admin@acme.test"}, "acme", "Acme")
	assert.NoError(t, err, "the portal works while read-only: it is how the invoice gets paid")

	// 28 days: suspension proposed, then confirmed by an operator
	*e.now = a.FailedAt.Add(28 * 24 * time.Hour)
	require.NoError(t, e.svc.DunningTick(ctx))
	views, err := e.svc.Accounts(ctx)
	require.NoError(t, err)
	require.Len(t, views, 1)
	assert.True(t, views[0].SuspensionProposed)
	assert.Equal(t, billing.AccessReadOnly, views[0].Access)
	require.NoError(t, e.svc.ConfirmSuspension(ctx, billing.Actor{Username: "op"}, "acme"))
	assert.Equal(t, []string{"acme"}, e.susp.suspended)
	assert.Equal(t, []string{"Procy billing: Payment failed", "Procy billing: Organization is read-only", "Procy billing: Organization may be suspended"}, e.notes.subjects())

	// payment succeeds: everything restored at once
	e.webhook(t, "evt_paid", "invoice.paid", invoice("in_1", "cus_1"))
	a, _ = e.store.Account(ctx, "acme")
	assert.Equal(t, billing.DunningNone, a.State)
	assert.Equal(t, billing.AccessFull, e.svc.Access(ctx, "acme"))
}

func TestOperatorRestore(t *testing.T) {
	e := newEnv(t)
	e.link("acme", "cus_1")
	e.webhook(t, "evt_f1", "invoice.payment_failed", invoice("in_1", "cus_1"))
	*e.now = e.now.Add(15 * 24 * time.Hour)
	a, _ := e.store.Account(ctx, "acme")
	*e.now = a.FailedAt.Add(15 * 24 * time.Hour)
	require.NoError(t, e.svc.DunningTick(ctx))
	require.Equal(t, billing.AccessReadOnly, e.svc.Access(ctx, "acme"))
	_, err := e.svc.Restore(ctx, billing.Actor{Username: "op"}, "acme")
	require.NoError(t, err)
	assert.Equal(t, billing.AccessFull, e.svc.Access(ctx, "acme"))
	assert.Contains(t, e.store.Audits, "op restore acme")
	_, err = e.svc.Restore(ctx, billing.Actor{Username: "op"}, "nobody")
	assert.ErrorIs(t, err, billing.ErrNotFound)
}

func TestDunningTickPure(t *testing.T) {
	failed := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	d := billing.Dunning{}.PaymentFailed(failed, "in_1")
	p := billing.DefaultDunningPolicy
	// a tick that didn't run for a month passes through read-only first
	d = d.Tick(failed.AddDate(0, 0, 40), p)
	assert.Equal(t, billing.DunningReadOnly, d.State)
	d = d.Tick(failed.AddDate(0, 0, 40), p)
	assert.Equal(t, billing.DunningSuspensionProposed, d.State)
	assert.Equal(t, billing.AccessReadOnly, d.Access())
	assert.NoError(t, d.CanConfirmSuspension())
	assert.Equal(t, billing.DunningNone, d.PaymentSucceeded().State)
	assert.Equal(t, billing.AccessFull, d.Restore().Access())
}

func period(org string, status billing.Status, state billing.PeriodState, extra, gib int64) billing.Period {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	return billing.Period{Organization: org, Start: start, End: start.AddDate(0, 1, 0), Plan: "starter", Status: status,
		ExtraDevices: extra, OverageGiB: gib, State: state, ClosedAt: start.AddDate(0, 1, 0).Add(2 * time.Hour)}
}

func TestReportingInArrearsIsIdempotentAndNeverSendsExempt(t *testing.T) {
	e := newEnv(t)
	for _, org := range []string{"acme", "zero", "held", "partner", "nowexempt", "trial"} {
		e.link(org, "cus_"+org)
	}
	e.records["nowexempt"] = &billing.Billing{Organization: "nowexempt", Status: billing.StatusExempt}
	*e.periods = periods{
		period("acme", billing.StatusPaying, billing.PeriodReady, 12, 21),
		period("zero", billing.StatusPaying, billing.PeriodReady, 0, 0),
		period("held", billing.StatusPaying, billing.PeriodHeld, 5, 900),
		period("partner", billing.StatusExempt, billing.PeriodNeverCharged, 390, 2000),
		// a period ready to be charged of an organization exempt now: still never sent
		period("nowexempt", billing.StatusPaying, billing.PeriodReady, 3, 3),
		period("trial", billing.StatusTrial, billing.PeriodNotCharged, 3, 0),
	}
	require.NoError(t, e.svc.ReportClosedPeriods(ctx))
	ev := e.fake.Events()
	require.Len(t, ev, 2)
	byID := map[string]stripetest.MeterEvent{}
	for _, x := range ev {
		byID[x.Identifier] = x
		assert.Equal(t, "cus_acme", x.Customer)
		assert.Equal(t, time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC), x.Timestamp, "usage is recorded inside the period")
	}
	assert.Equal(t, int64(12), byID["procy:acme:extra_devices:2026-11-01"].Value)
	assert.Equal(t, "procy_extra_devices", byID["procy:acme:extra_devices:2026-11-01"].EventName)
	assert.Equal(t, int64(21), byID["procy:acme:overage_gib:2026-11-01"].Value)
	assert.ElementsMatch(t, []string{"acme 2026-11", "zero 2026-11"}, e.sync.reported)

	// running again sends nothing more
	require.NoError(t, e.svc.ReportClosedPeriods(ctx))
	assert.Len(t, e.fake.Events(), 2)
	for _, r := range e.store.Rpts {
		assert.Equal(t, billing.ReportSent, r.State)
		assert.Equal(t, 1, r.Attempts)
	}
}

func TestReportingRetriesFailures(t *testing.T) {
	e := newEnv(t)
	e.link("acme", "cus_acme")
	*e.periods = periods{period("acme", billing.StatusPaying, billing.PeriodReady, 12, 0)}
	e.fake.FailMeterEvents = true
	_ = e.svc.ReportClosedPeriods(ctx)
	r := e.store.Rpts["procy:acme:extra_devices:2026-11-01"]
	assert.Equal(t, billing.ReportFailed, r.State)
	assert.Empty(t, e.sync.reported)

	// too soon: not retried
	e.fake.FailMeterEvents = false
	*e.now = e.now.Add(time.Minute)
	require.NoError(t, e.svc.ReportClosedPeriods(ctx))
	assert.Empty(t, e.fake.Events())

	*e.now = e.now.Add(15 * time.Minute)
	require.NoError(t, e.svc.ReportClosedPeriods(ctx))
	assert.Len(t, e.fake.Events(), 1)
	r = e.store.Rpts["procy:acme:extra_devices:2026-11-01"]
	assert.Equal(t, billing.ReportSent, r.State)
	assert.Equal(t, 2, r.Attempts)
	assert.Equal(t, []string{"acme 2026-11"}, e.sync.reported)
}

func TestReportingAfterTheDedupWindowLooksFirst(t *testing.T) {
	e := newEnv(t)
	e.link("acme", "cus_acme")
	*e.periods = periods{period("acme", billing.StatusPaying, billing.PeriodReady, 12, 0)}
	// a send that "failed" (timeout) but reached Stripe a day ago
	require.NoError(t, e.svc.ReportClosedPeriods(ctx))
	r := e.store.Rpts["procy:acme:extra_devices:2026-11-01"]
	long := e.now.Add(-25 * time.Hour)
	r.State, r.LastAttemptAt, r.SentAt = billing.ReportFailed, &long, nil
	e.store.Rpts[r.Identifier] = r
	e.fake.MeterEvents = e.fake.MeterEvents[:1]
	e.fake.MeterEvents[0].Identifier = "procy:acme:extra_devices:2026-11-01#stripe-forgot-the-identifier"
	require.NoError(t, e.svc.ReportClosedPeriods(ctx))
	assert.Len(t, e.fake.Events(), 1, "the provider shows the usage: not sent again")
	assert.Equal(t, billing.ReportSent, e.store.Rpts[r.Identifier].State)
}

func TestReconciliation(t *testing.T) {
	e := newEnv(t)
	e.link("acme", "cus_acme")
	e.link("beta", "cus_beta")
	e.link("gamma", "cus_gamma")
	*e.periods = periods{
		period("acme", billing.StatusPaying, billing.PeriodReady, 12, 0),
		period("beta", billing.StatusPaying, billing.PeriodReady, 4, 0),
		period("gamma", billing.StatusPaying, billing.PeriodReady, 7, 0),
	}
	require.NoError(t, e.svc.ReportClosedPeriods(ctx))
	require.Len(t, e.fake.Events(), 3)
	// beta's event never counted at Stripe; gamma's counted differently
	e.fake.MeterEvents = []stripetest.MeterEvent{e.fake.MeterEvents[0], e.fake.MeterEvents[2]}
	e.fake.MeterEvents[1].Value = 9

	// too early to compare
	require.NoError(t, e.svc.Reconcile(ctx))
	assert.Equal(t, billing.ReportSent, e.store.Rpts["procy:acme:extra_devices:2026-11-01"].State)

	*e.now = e.now.Add(2 * time.Hour)
	require.NoError(t, e.svc.Reconcile(ctx))
	assert.Equal(t, billing.ReportConfirmed, e.store.Rpts["procy:acme:extra_devices:2026-11-01"].State)
	assert.Equal(t, billing.ReportMismatch, e.store.Rpts["procy:gamma:extra_devices:2026-11-01"].State)
	assert.Equal(t, 9.0, *e.store.Rpts["procy:gamma:extra_devices:2026-11-01"].ProviderValue)
	assert.Equal(t, billing.ReportSent, e.store.Rpts["procy:beta:extra_devices:2026-11-01"].State, "nothing at Stripe yet: wait")
	assert.Len(t, e.fake.Events(), 2)

	*e.now = e.now.Add(6 * time.Hour)
	require.NoError(t, e.svc.Reconcile(ctx))
	assert.Len(t, e.fake.Events(), 3, "still nothing after hours: sent again with the same identifier")
	assert.Equal(t, "procy:beta:extra_devices:2026-11-01", e.fake.Events()[2].Identifier)

	*e.now = e.now.Add(2 * time.Hour)
	require.NoError(t, e.svc.Reconcile(ctx))
	assert.Equal(t, billing.ReportConfirmed, e.store.Rpts["procy:beta:extra_devices:2026-11-01"].State)
	assert.Equal(t, int64(1), e.svc.Stats().Mismatches)
}

func TestCheckout(t *testing.T) {
	e := newEnv(t)
	e.store.Emails["acme"] = []string{"admin@acme.test"}
	res, err := e.svc.Checkout(ctx, billing.Actor{Username: "admin@acme.test"}, "acme", "Acme Inc", "starter")
	require.NoError(t, err)
	assert.Contains(t, res.URL, "https://checkout.stripe.test/")
	a, _ := e.store.Account(ctx, "acme")
	require.NotNil(t, a)
	assert.NotEmpty(t, a.CustomerID)
	assert.Equal(t, "admin@acme.test", a.BillingEmail)
	form := e.fake.LastForm("/v1/checkout/sessions")
	assert.Equal(t, "https://procy.test/billing?checkout=success", form.Get("success_url"))
	assert.Equal(t, "1798761600", form.Get("subscription_data[billing_cycle_anchor]"), "anchored at 2027-01-01")

	// a second checkout reuses the customer
	_, err = e.svc.Checkout(ctx, billing.Actor{Username: "admin@acme.test"}, "acme", "Acme Inc", "starter")
	require.NoError(t, err)
	assert.Equal(t, 1, e.fake.Count("POST /v1/customers"))

	// with a live subscription the plan changes in place
	e.fake.Subscriptions["sub_1"] = &stripetest.Subscription{ID: "sub_1", Customer: a.CustomerID, Status: "active", Items: []string{"price_base"}}
	a.SubscriptionID, a.SubscriptionStatus = "sub_1", billing.SubscriptionActive
	require.NoError(t, e.store.PutAccount(ctx, a))
	res, err = e.svc.Checkout(ctx, billing.Actor{Username: "admin@acme.test"}, "acme", "Acme Inc", "team")
	require.NoError(t, err)
	assert.True(t, res.PlanChanged)
	assert.Equal(t, []string{"price_team", "price_devices"}, e.fake.Subscriptions["sub_1"].Items)

	_, err = e.svc.Checkout(ctx, billing.Actor{Username: "a"}, "acme", "", "free")
	assert.ErrorIs(t, err, billing.ErrConflict)
	_, err = e.svc.Checkout(ctx, billing.Actor{Username: "a"}, "acme", "", "nope")
	assert.ErrorIs(t, err, billing.ErrNotFound)
}

func TestCheckoutDuringFreeTimeChargesNothingBeforeItEnds(t *testing.T) {
	e := newEnv(t)
	// a trial ending this month: anchored at the 1st without prorations
	ends := e.now.AddDate(0, 0, 10)
	e.records["acme"] = &billing.Billing{Organization: "acme", Status: billing.StatusTrial, TrialEndsAt: &ends}
	_, err := e.svc.Checkout(ctx, billing.Actor{Username: "admin@acme.test"}, "acme", "Acme", "starter")
	require.NoError(t, err)
	form := e.fake.LastForm("/v1/checkout/sessions")
	assert.Equal(t, "1798761600", form.Get("subscription_data[billing_cycle_anchor]"))
	assert.Equal(t, "none", form.Get("subscription_data[proration_behavior]"))
	assert.Empty(t, form.Get("subscription_data[trial_end]"), "Checkout takes an anchor or a trial, never both")

	// comped for months: a trial until the day after the comped date
	until := e.now.AddDate(0, 3, 0)
	e.records["beta"] = &billing.Billing{Organization: "beta", Status: billing.StatusComped, CompedUntil: &until}
	_, err = e.svc.Checkout(ctx, billing.Actor{Username: "admin@beta.test"}, "beta", "Beta", "starter")
	require.NoError(t, err)
	form = e.fake.LastForm("/v1/checkout/sessions")
	assert.Equal(t, strconv.FormatInt(billing.DayStart(until).AddDate(0, 0, 1).Unix(), 10), form.Get("subscription_data[trial_end]"))
	assert.Empty(t, form.Get("subscription_data[billing_cycle_anchor]"))
	assert.Empty(t, form.Get("subscription_data[proration_behavior]"))
}

func TestExemptOrganizationsAreNeverSentToTheProvider(t *testing.T) {
	e := newEnv(t)
	e.records["partner"] = &billing.Billing{Organization: "partner", Status: billing.StatusExempt}
	_, err := e.svc.Checkout(ctx, billing.Actor{Username: "admin@partner.test"}, "partner", "Partner", "starter")
	assert.ErrorIs(t, err, billing.ErrConflict)
	_, err = e.svc.PortalURL(ctx, billing.Actor{Username: "admin@partner.test"}, "partner", "Partner")
	assert.ErrorIs(t, err, billing.ErrConflict)
	assert.Zero(t, e.fake.Count("POST /v1/customers"))
	v, err := e.svc.Payment(ctx, "partner")
	require.NoError(t, err)
	assert.False(t, v.Chargeable)
	inv, err := e.svc.Invoices(ctx, "partner", 10)
	require.NoError(t, err)
	assert.Empty(t, inv)
}

func TestCheckoutCompletedAndCanceled(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.Checkout(ctx, billing.Actor{Username: "admin@acme.test"}, "acme", "Acme", "starter")
	require.NoError(t, err)
	a, _ := e.store.Account(ctx, "acme")
	e.webhook(t, "evt_cs", "checkout.session.completed", map[string]any{"id": "cs_1", "object": "checkout.session", "mode": "subscription",
		"status": "complete", "customer": a.CustomerID, "subscription": "sub_new", "client_reference_id": "acme"})
	a, _ = e.store.Account(ctx, "acme")
	assert.Equal(t, "sub_new", a.SubscriptionID)
	assert.True(t, a.HasPaymentMethod)

	sub := e.fake.SubscriptionJSON(&stripetest.Subscription{ID: "sub_new", Customer: a.CustomerID, Status: "canceled", Items: []string{"price_base"}})
	e.webhook(t, "evt_del", "customer.subscription.deleted", sub)
	a, _ = e.store.Account(ctx, "acme")
	assert.Equal(t, billing.SubscriptionCanceled, a.SubscriptionStatus)
	assert.Contains(t, e.sync.states, "acme sub_new plan=free pm", "a canceled subscription moves to the free plan")
	assert.Equal(t, billing.AccessFull, e.svc.Access(ctx, "acme"), "canceling never suspends")
}

func TestTrialWillEndNotifies(t *testing.T) {
	e := newEnv(t)
	e.link("acme", "cus_1")
	sub := e.fake.SubscriptionJSON(&stripetest.Subscription{ID: "sub_acme", Customer: "cus_1", Status: "trialing"})
	e.webhook(t, "evt_t", "customer.subscription.trial_will_end", sub)
	assert.Equal(t, []string{"Procy billing: Your trial ends soon"}, e.notes.subjects())
}

var createdRe = regexp.MustCompile(`"created":\d+`)

func replaceCreated(body string, at int64) string {
	return createdRe.ReplaceAllString(body, `"created":`+strconv.FormatInt(at, 10))
}
