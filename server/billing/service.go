package billing

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/tenant"
)

// Intervals of the payment provider's background jobs, each run by one node at a time (a lease).
const (
	// ReportInterval: closed periods are reported soon after they close, well inside the provider's invoice
	// finalization grace period.
	ReportInterval = 15 * time.Minute
	// ReconcileInterval: reported usage is compared with the provider's summaries daily.
	ReconcileInterval = 24 * time.Hour
	// DunningInterval: overdue organizations move on to read-only and suspension proposed.
	DunningInterval = time.Hour
	// DefaultFinalizationGrace is how long after a period ends its invoice still takes usage: the provider's
	// invoice finalization grace period, which the operators set (Stripe: 1 hour by default; the docs ask for at
	// least 6). finalizationMargin keeps a send clear of it.
	DefaultFinalizationGrace = 6 * time.Hour
	finalizationMargin       = 15 * time.Minute
	// accessCacheTTL is how long an organization's access is cached for the read-only middleware.
	accessCacheTTL = 30 * time.Second
	// reportLookback is how far back closed periods are looked at (the provider takes usage up to 35 days old).
	reportLookback = 40 * 24 * time.Hour
	// retryAfter is how long a failed send waits before it is retried.
	retryAfter = 10 * time.Minute
	// dedupWindow is how long the provider deduplicates an identifier (Stripe: at least 24 hours), with margin:
	// after it, a record is sent again only when the provider's summary shows nothing.
	dedupWindow = 20 * time.Hour
	// confirmAfter is how long after sending the summary is first compared (the provider aggregates
	// asynchronously); resendAfter how long a summary of 0 is waited for before the record is sent again.
	confirmAfter = time.Hour
	resendAfter  = 6 * time.Hour
)

// WebhookActor is the actor of webhook-driven changes in the audit log.
func WebhookActor(provider string) Actor { return Actor{Username: "billing:" + provider} }

// Suspender suspends an organization (tenants.status = suspended, through the operator service, audited there).
type Suspender interface {
	Suspend(ctx context.Context, by Actor, org string) error
}

// Sync passes what the provider learned to the organization's billing record (billing-plans' org_billing:
// has_payment_method, provider ids, the plan bought, periods reported). Optional.
type Sync interface {
	// ProviderState records the customer, subscription and payment method and, when plan is set, the plan the
	// organization bought (the plan of the subscription's prices; the free plan once it is canceled). An exempt
	// organization's plan is never changed this way.
	ProviderState(ctx context.Context, org, customerID, subscriptionID string, hasPaymentMethod bool, plan string) error
	// PeriodReported marks the period starting start as reported.
	PeriodReported(ctx context.Context, org string, start, at time.Time) error
}

// Options are the service's dependencies. Provider, Store and Plans are required.
type Options struct {
	Provider Provider
	Store    Store
	Plans    PlanCatalog
	// PlanIDs lists the plans (to find the plan of a subscription's prices); FreePlan is where a canceled
	// subscription moves the organization.
	PlanIDs  []string
	FreePlan string
	// Records reads billing statuses (exempt organizations are never sent to the provider). Nil: every
	// organization is paying.
	Records RecordReader
	// Periods lists closed periods to report. Nil: nothing is reported.
	Periods ClosedPeriods
	// Sync, Notifier and Suspender are optional.
	Sync      Sync
	Notifier  Notifier
	Suspender Suspender
	Dunning   DunningPolicy
	// FinalizationGrace is the provider's invoice finalization grace period (default DefaultFinalizationGrace).
	FinalizationGrace time.Duration
	// PruneEvents, when set, prunes old webhook event ids (run with the reconciliation).
	PruneEvents func(ctx context.Context) error
	// ReturnURL is the UI's billing page: checkout and the portal return there.
	ReturnURL string
	Log       *logger.Logger
	Now       func() time.Time
}

// Service is the payment provider integration: webhooks, checkout and portal, usage reporting in arrears,
// reconciliation and dunning.
type Service struct {
	o   Options
	now func() time.Time
	log *logger.Logger

	mu       sync.Mutex
	access   map[string]accessEntry
	notifier Notifier

	// counters for operators
	statsMu sync.Mutex
	stats   Stats
}

type accessEntry struct {
	access Access
	at     time.Time
}

// Stats are the service's counters since start.
type Stats struct {
	WebhooksAccepted  int64 `json:"webhooks_accepted"`
	WebhooksDuplicate int64 `json:"webhooks_duplicate"`
	// WebhooksUnverified are requests refused for their signature (400).
	WebhooksUnverified int64 `json:"webhooks_unverified"`
	WebhooksFailed     int64 `json:"webhooks_failed"`
	ReportsSent        int64 `json:"reports_sent"`
	ReportsFailed      int64 `json:"reports_failed"`
	Mismatches         int64 `json:"mismatches"`
}

// NewService returns the service.
func NewService(o Options) (*Service, error) {
	if o.Provider == nil || o.Store == nil || o.Plans == nil {
		return nil, errors.New("billing: the service needs a provider, a store and the plans")
	}
	if o.Dunning.Grace <= 0 {
		o.Dunning.Grace = DefaultDunningPolicy.Grace
	}
	if o.FinalizationGrace <= 0 {
		o.FinalizationGrace = DefaultFinalizationGrace
	}
	if o.Dunning.ReadOnly <= 0 {
		o.Dunning.ReadOnly = DefaultDunningPolicy.ReadOnly
	}
	if o.Log == nil {
		o.Log = logger.NewLogger("billing", logger.LogOutput{}, logger.LogLevelError)
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Service{o: o, now: func() time.Time { return now().UTC() }, log: o.Log, access: map[string]accessEntry{}, notifier: o.Notifier}, nil
}

// SetNotifier sets how administrators are notified (the server's notifications exist only once its API does).
func (s *Service) SetNotifier(n Notifier) {
	s.mu.Lock()
	s.notifier = n
	s.mu.Unlock()
}

func (s *Service) currentNotifier() Notifier {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notifier
}

// ProviderName is the provider's name.
func (s *Service) ProviderName() string { return s.o.Provider.Name() }

// Stats returns the counters.
func (s *Service) Stats() Stats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.stats
}

func (s *Service) count(f func(*Stats)) {
	s.statsMu.Lock()
	f(&s.stats)
	s.statsMu.Unlock()
}

func (s *Service) record(ctx context.Context, org string) (*Billing, error) {
	if s.o.Records == nil {
		return &Billing{Organization: org, Status: StatusPaying}, nil
	}
	b, err := s.o.Records.Billing(ctx, org)
	if err != nil {
		return nil, err
	}
	if b == nil {
		b = &Billing{Organization: org, Status: StatusPaying}
	}
	return b, nil
}

// planOfPrices returns the plan whose base (or, without one, devices) price is among ids, or "".
func (s *Service) planOfPrices(plans []string, ids []string) string {
	has := map[string]bool{}
	for _, id := range ids {
		has[id] = true
	}
	for _, id := range plans {
		pl := s.o.Plans.Get(id)
		if pl == nil {
			continue
		}
		for _, k := range []string{PriceBase, PriceDevices} {
			if p := pl.ProviderPrices[k]; p != "" {
				if has[p] {
					return pl.ID
				}
				break
			}
		}
	}
	return ""
}

// ---- webhooks ----

// WebhookResult is the outcome of a webhook request.
type WebhookResult int

const (
	// WebhookApplied: a new event was applied (200).
	WebhookApplied WebhookResult = iota
	// WebhookDuplicate: the event was received before (200, nothing changes).
	WebhookDuplicate
	// WebhookUnverified: the request is not from the provider (400).
	WebhookUnverified
	// WebhookUnreadable: a verified request the integration can't read (another API version; 400, the provider
	// retries until the endpoint is fixed).
	WebhookUnreadable
	// WebhookFailed: the event couldn't be applied; the provider retries (500).
	WebhookFailed
)

// SystemTenant is the tenant the webhook and the background jobs work in (operator.SystemTenant).
const SystemTenant tenant.ID = "_system"

// HandleWebhook verifies and applies a webhook request.
func (s *Service) HandleWebhook(ctx context.Context, body []byte, header map[string][]string) (WebhookResult, error) {
	e, err := s.o.Provider.ParseWebhook(body, header)
	if err != nil {
		if errors.Is(err, ErrSignature) {
			s.count(func(st *Stats) { st.WebhooksUnverified++ })
			return WebhookUnverified, err
		}
		s.count(func(st *Stats) { st.WebhooksFailed++ })
		return WebhookUnreadable, err
	}
	applied, err := s.ProcessEvent(ctx, e)
	switch {
	case err != nil:
		s.count(func(st *Stats) { st.WebhooksFailed++ })
		return WebhookFailed, err
	case !applied:
		s.count(func(st *Stats) { st.WebhooksDuplicate++ })
		return WebhookDuplicate, nil
	}
	s.count(func(st *Stats) { st.WebhooksAccepted++ })
	return WebhookApplied, nil
}

// ProcessEvent applies a verified event once, in the system tenant. The organization is found through our own
// customer mapping; an organization the provider names (metadata, client reference) must agree with it, or the
// event is recorded and ignored. The event is claimed, the account changed and the billing record synced in one
// transaction: a failure leaves the event unclaimed and the provider sends it again.
func (s *Service) ProcessEvent(ctx context.Context, e *Event) (applied bool, err error) {
	ctx = tenant.WithID(ctx, SystemTenant)
	provider := s.o.Provider.Name()
	org := ""
	if e.CustomerID != "" {
		if org, err = s.o.Store.OrganizationOf(ctx, provider, e.CustomerID); err != nil {
			return false, err
		}
	}
	if org == "" && e.Type != EventIgnored {
		s.log.Errorf("billing: %s event %s (%s) for customer %q of no organization: ignored", provider, e.ID, e.ProviderType, e.CustomerID)
	}
	if org != "" && e.Organization != "" && e.Organization != org {
		s.log.Errorf("billing: %s event %s names organization %q but customer %s is %q's: ignored", provider, e.ID, e.Organization, e.CustomerID, org)
		org = ""
	}
	var exempt bool
	if org != "" {
		b, err := s.record(ctx, org)
		if err != nil {
			return false, err
		}
		exempt = b.Status == StatusExempt
	}
	// a failure delivered after the invoice was paid (or voided) is stale: look the invoice up first
	staleFailure := false
	if org != "" && e.Type == EventPaymentFailed && e.InvoiceID != "" {
		st, err := s.o.Provider.InvoiceStatus(ctx, e.InvoiceID)
		if err != nil {
			return false, err
		}
		staleFailure = st == "paid" || st == "void"
	}
	var before, after *ProviderAccount
	var duplicate string
	applied, err = s.o.Store.ApplyEvent(ctx, provider, e.ID, e.ProviderType, org, func(cur *ProviderAccount) (*ProviderAccount, error) {
		before, after, duplicate = nil, nil, ""
		if org == "" || cur == nil || e.Type == EventIgnored {
			return nil, nil
		}
		if e.Type == EventPaymentFailed && (exempt || staleFailure) {
			if exempt && e.Type == EventPaymentFailed {
				s.log.Errorf("billing: a payment of exempt organization %s failed (%s); exempt organizations are never in dunning", org, e.InvoiceID)
			}
			return nil, nil
		}
		if isSecondSubscription(cur, e) {
			duplicate = e.SubscriptionID
			return nil, nil
		}
		prev := *cur
		before = &prev
		next := *cur
		next.UpdatedBy = WebhookActor(provider).Username + " " + e.ID
		if !applyEvent(&next, e) {
			return nil, nil
		}
		after = &next
		if err := s.sync(ctx, e, &next); err != nil {
			return nil, err
		}
		return &next, nil
	})
	if err != nil || !applied {
		return applied, err
	}
	if duplicate != "" {
		s.cancelDuplicate(ctx, org, duplicate, e)
	}
	if after == nil {
		return true, nil
	}
	s.forget(org)
	s.afterEvent(ctx, e, before, after)
	return true, nil
}

// isSecondSubscription reports whether e starts another subscription while a's is still live (two checkout pages
// completed): it must not replace the first, or both would bill.
func isSecondSubscription(a *ProviderAccount, e *Event) bool {
	if a.SubscriptionID == "" || e.SubscriptionID == "" || e.SubscriptionID == a.SubscriptionID {
		return false
	}
	if a.SubscriptionStatus != "" && !live(a.SubscriptionStatus) {
		return false
	}
	switch e.Type {
	case EventCheckoutCompleted:
		return true
	case EventSubscriptionChanged:
		return e.SubscriptionStatus == "" || live(e.SubscriptionStatus) || e.SubscriptionStatus == SubscriptionIncomplete
	}
	return false
}

// cancelDuplicate cancels a second subscription at once and tells operators (its first invoice may need a refund).
func (s *Service) cancelDuplicate(ctx context.Context, org, subscriptionID string, e *Event) {
	s.log.Errorf("billing: organization %s completed a second subscription %s while another is live; canceling it (check its first invoice for a refund)", org, subscriptionID)
	if err := s.o.Provider.Cancel(ctx, subscriptionID, false); err != nil {
		s.log.Errorf("billing: cancel the second subscription %s of %s: %v", subscriptionID, org, err)
	}
	if err := s.o.Store.Audit(ctx, WebhookActor(s.o.Provider.Name()), AuditCancel, org,
		map[string]string{"subscription": subscriptionID, "event": e.ID, "reason": "second live subscription"}); err != nil {
		s.log.Errorf("billing: %v", err)
	}
}

// sync passes the account's provider state to the billing record (inside the event's transaction: a failure makes
// the provider send the event again). The plan follows the subscription only while it bills (active, trialing);
// a subscription that ended (canceled, incomplete_expired) moves the organization to the free plan.
func (s *Service) sync(ctx context.Context, e *Event, a *ProviderAccount) error {
	if s.o.Sync == nil {
		return nil
	}
	switch e.Type {
	case EventCheckoutCompleted, EventSubscriptionChanged, EventSubscriptionCanceled, EventInvoicePaid:
	default:
		return nil
	}
	plan := ""
	switch a.SubscriptionStatus {
	case SubscriptionCanceled, SubscriptionIncompleteExpired:
		plan = s.o.FreePlan
	case SubscriptionActive, SubscriptionTrialing:
		if e.Type == EventSubscriptionChanged {
			plan = s.planOfPrices(s.o.PlanIDs, a.PriceIDs)
		}
	}
	return s.o.Sync.ProviderState(ctx, a.Organization, a.CustomerID, a.SubscriptionID, a.HasPaymentMethod, plan)
}

// applyEvent changes a according to e; false when nothing changes.
func applyEvent(a *ProviderAccount, e *Event) bool {
	switch e.Type {
	case EventCheckoutCompleted:
		if e.SubscriptionID != "" {
			a.SubscriptionID = e.SubscriptionID
		}
		if e.SubscriptionStatus != "" {
			a.SubscriptionStatus = e.SubscriptionStatus
		}
		a.HasPaymentMethod = a.HasPaymentMethod || e.PaymentMethod
		return true
	case EventSubscriptionChanged, EventSubscriptionCanceled:
		if a.SubscriptionID != "" && e.SubscriptionID != a.SubscriptionID && e.Type == EventSubscriptionCanceled {
			// an old subscription ending doesn't end the current one
			return false
		}
		// events arrive out of order: an older one doesn't overwrite a newer state of the same subscription
		if a.SubscriptionEventAt != nil && e.Created.Before(*a.SubscriptionEventAt) && e.SubscriptionID == a.SubscriptionID {
			return false
		}
		at := e.Created
		a.SubscriptionEventAt = &at
		a.SubscriptionID = e.SubscriptionID
		a.SubscriptionStatus = e.SubscriptionStatus
		if e.Type == EventSubscriptionCanceled {
			a.SubscriptionStatus = SubscriptionCanceled
		}
		a.PriceIDs = e.PriceIDs
		return true
	case EventInvoicePaid:
		a.HasPaymentMethod = a.HasPaymentMethod || e.PaymentMethod
		// only the invoice that failed ends the episode (not a $0 or one-off invoice paid meanwhile)
		if a.State != DunningNone && a.State != "" && (a.FailedInvoice == "" || e.InvoiceID == a.FailedInvoice) {
			a.Dunning = a.PaymentSucceeded()
		}
		return true
	case EventPaymentFailed:
		a.Dunning = a.PaymentFailed(e.Created, e.InvoiceID)
		return true
	case EventTrialWillEnd:
		return true
	}
	return false
}

func (s *Service) afterEvent(ctx context.Context, e *Event, before, after *ProviderAccount) {
	org := after.Organization
	by := WebhookActor(s.o.Provider.Name())
	if err := s.o.Store.Audit(ctx, by, AuditWebhook, org, map[string]any{
		"event": e.ID, "type": e.ProviderType, "subscription_status": after.SubscriptionStatus, "dunning_state": after.State,
	}); err != nil {
		s.log.Errorf("billing: %v", err)
	}
	switch {
	case before.State != DunningPastDue && after.State == DunningPastDue:
		s.notifyOnce(ctx, org, "payment_failed", episode(after), "Payment failed",
			fmt.Sprintf("A payment for organization %s failed. Service continues in full for %d days while the payment is retried. "+
				"Update the payment method on the Billing page%s.", org, int(s.o.Dunning.Grace.Hours()/24), s.billingLink()))
	case before.State != DunningNone && before.State != "" && after.State == DunningNone && e.Type == EventInvoicePaid:
		s.notifyOnce(ctx, org, "payment_succeeded", episode(before), "Payment received",
			fmt.Sprintf("The overdue payment for organization %s was received. Everything is restored.", org))
	}
	if e.Type == EventTrialWillEnd {
		s.notifyOnce(ctx, org, "trial_will_end", e.SubscriptionID, "Your trial ends soon",
			fmt.Sprintf("The trial of organization %s ends in a few days. Add a payment method on the Billing page%s to keep your plan.", org, s.billingLink()))
	}
}

// episode names a dunning episode by its first failure.
func episode(a *ProviderAccount) string {
	if a.FailedAt == nil {
		return "none"
	}
	return a.FailedAt.UTC().Format(time.RFC3339)
}

func (s *Service) billingLink() string {
	if s.o.ReturnURL == "" {
		return ""
	}
	return " (" + s.o.ReturnURL + ")"
}

// notifyOnce notifies org's administrators once per kind and episode.
func (s *Service) notifyOnce(ctx context.Context, org, kind, ep, subject, body string) {
	n := s.currentNotifier()
	if n == nil {
		return
	}
	ok, err := s.o.Store.ClaimNotice(ctx, org, kind, ep)
	if err != nil {
		s.log.Errorf("billing: %v", err)
		return
	}
	if !ok {
		return
	}
	to, err := s.o.Store.AdminEmails(ctx, org)
	if err != nil {
		s.log.Errorf("billing: %v", err)
		return
	}
	if a, err := s.o.Store.Account(ctx, org); err == nil && a != nil && a.BillingEmail != "" && !contains(to, a.BillingEmail) {
		to = append(to, a.BillingEmail)
	}
	if len(to) == 0 {
		s.log.Errorf("billing: no administrator email address to notify %s of %s", org, kind)
		return
	}
	if err := n.Notify(ctx, tenant.ID(org), to, "Procy billing: "+subject, body); err != nil {
		s.log.Errorf("billing: notify %s of %s: %v", org, kind, err)
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// ---- access (read-only) ----

// Access returns org's access (cached for a short time; full when it can't be read: never refuse because of a
// database hiccup).
func (s *Service) Access(ctx context.Context, org string) Access {
	s.mu.Lock()
	e, ok := s.access[org]
	s.mu.Unlock()
	if ok && s.now().Sub(e.at) < accessCacheTTL {
		return e.access
	}
	a, err := s.o.Store.Account(ctx, org)
	if err != nil {
		s.log.Errorf("billing: access of %s: %v", org, err)
		return AccessFull
	}
	acc := a.Access()
	s.mu.Lock()
	s.access[org] = accessEntry{access: acc, at: s.now()}
	s.mu.Unlock()
	return acc
}

func (s *Service) forget(org string) {
	s.mu.Lock()
	delete(s.access, org)
	s.mu.Unlock()
}

// ---- organization administrators ----

// PaymentView is what an organization's administrators see of the provider (GET /billing/payment).
type PaymentView struct {
	Provider string `json:"provider"`
	Plan     string `json:"plan"`
	Status   Status `json:"status"`
	// Chargeable is false for exempt organizations: they have no provider records and no checkout.
	Chargeable         bool               `json:"chargeable"`
	HasCustomer        bool               `json:"has_customer"`
	HasPaymentMethod   bool               `json:"has_payment_method"`
	SubscriptionStatus SubscriptionStatus `json:"subscription_status"`
	CancelAtPeriodEnd  bool               `json:"cancel_at_period_end"`
	Access             Access             `json:"access"`
	Dunning            Dunning            `json:"dunning"`
	// ReadOnlyFrom is when the organization becomes read-only if the payment doesn't succeed (past due).
	ReadOnlyFrom *time.Time `json:"read_only_from,omitempty"`
}

// Payment returns org's PaymentView.
func (s *Service) Payment(ctx context.Context, org string) (*PaymentView, error) {
	b, err := s.record(ctx, org)
	if err != nil {
		return nil, err
	}
	a, err := s.o.Store.Account(ctx, org)
	if err != nil {
		return nil, err
	}
	v := &PaymentView{Provider: s.o.Provider.Name(), Plan: b.Plan, Status: b.Status, Chargeable: b.Status != StatusExempt,
		Access: AccessFull, Dunning: Dunning{State: DunningNone}}
	if a != nil {
		v.HasCustomer = a.CustomerID != ""
		v.HasPaymentMethod = a.HasPaymentMethod
		v.SubscriptionStatus = a.SubscriptionStatus
		v.Access = a.Access()
		v.Dunning = a.Dunning
		if v.Dunning.State == "" {
			v.Dunning.State = DunningNone
		}
		if a.State == DunningPastDue && a.FailedAt != nil {
			t := a.FailedAt.Add(s.o.Dunning.Grace)
			v.ReadOnlyFrom = &t
		}
	}
	return v, nil
}

// returnURL is the billing page with query q added.
func (s *Service) returnURL(q string) string {
	u := s.o.ReturnURL
	if q == "" {
		return u
	}
	if strings.Contains(u, "?") {
		return u + "&" + q
	}
	return u + "?" + q
}

// ensureCustomer returns org's account with a provider customer, creating it when needed. Exempt organizations
// are refused: they are never sent to the provider.
func (s *Service) ensureCustomer(ctx context.Context, org, name, email string) (*ProviderAccount, *Billing, error) {
	b, err := s.record(ctx, org)
	if err != nil {
		return nil, nil, err
	}
	if b.Status == StatusExempt {
		return nil, nil, fmt.Errorf("%w: organization %s is exempt from billing", ErrConflict, org)
	}
	a, err := s.o.Store.Account(ctx, org)
	if err != nil {
		return nil, nil, err
	}
	existing := ""
	if a != nil {
		existing = a.CustomerID
	} else {
		a = &ProviderAccount{Organization: org, Provider: s.o.Provider.Name(), Dunning: Dunning{State: DunningNone}}
	}
	id, err := s.o.Provider.EnsureCustomer(ctx, Customer{Organization: org, Name: name, Email: email}, existing)
	if err != nil {
		return nil, nil, err
	}
	if id == existing && (a.BillingEmail != "" || email == "") {
		return a, b, nil
	}
	// written under the account's lock, onto what is there now (a webhook may have changed it meanwhile)
	var out *ProviderAccount
	err = s.o.Store.UpdateAccount(ctx, org, func(cur *ProviderAccount) (*ProviderAccount, error) {
		if cur == nil {
			cur = &ProviderAccount{Organization: org, Provider: s.o.Provider.Name(), Dunning: Dunning{State: DunningNone}}
		}
		if cur.CustomerID != existing && cur.CustomerID != "" {
			// another request linked a customer first: keep it (ours is left unused)
			out = cur
			return nil, nil
		}
		cur.CustomerID = id
		if cur.BillingEmail == "" {
			cur.BillingEmail = email
		}
		cur.UpdatedBy = email
		out = cur
		return cur, nil
	})
	if err != nil {
		return nil, nil, err
	}
	if s.o.Sync != nil {
		if err := s.o.Sync.ProviderState(ctx, org, out.CustomerID, out.SubscriptionID, out.HasPaymentMethod, ""); err != nil {
			s.log.Errorf("billing: sync %s: %v", org, err)
		}
	}
	return out, b, nil
}

// CheckoutResult is the answer to a checkout: a URL to send the user to, or a plan changed in place (an active
// subscription moves to the new plan's prices).
type CheckoutResult struct {
	URL         string `json:"url,omitempty"`
	PlanChanged bool   `json:"plan_changed,omitempty"`
}

// Checkout starts buying plan for org: the provider's checkout when org has no live subscription, otherwise the
// subscription moves to the plan's prices. by is the administrator (email or username).
func (s *Service) Checkout(ctx context.Context, by Actor, org, orgName, planID string) (*CheckoutResult, error) {
	plan := s.o.Plans.Get(planID)
	if plan == nil {
		return nil, fmt.Errorf("%w: no plan %q", ErrNotFound, planID)
	}
	if len(plan.ProviderPrices) == 0 {
		return nil, fmt.Errorf("%w: plan %q is not sold through the payment provider", ErrConflict, planID)
	}
	if s.Access(ctx, org) == AccessReadOnly {
		// the portal pays the overdue invoice; a new plan doesn't
		return nil, ErrReadOnly
	}
	email := ""
	if strings.Contains(by.Username, "@") {
		email = by.Username
	}
	a, b, err := s.ensureCustomer(ctx, org, orgName, email)
	if err != nil {
		return nil, err
	}
	// a subscription the provider holds, whether or not its webhook arrived yet: never start a second one
	subs, err := s.o.Provider.Subscriptions(ctx, a.CustomerID)
	if err != nil {
		return nil, err
	}
	for _, sub := range subs {
		switch {
		case sub.ID == a.SubscriptionID && live(sub.Status):
		case sub.Status == SubscriptionIncomplete:
			return nil, fmt.Errorf("%w: a checkout is completing (its first payment is pending); try again in a few minutes", ErrConflict)
		default:
			return nil, fmt.Errorf("%w: the organization already has subscription %s (%s); manage it in the customer portal", ErrConflict, sub.ID, sub.Status)
		}
	}
	if a.SubscriptionID != "" && live(a.SubscriptionStatus) && len(subs) > 0 {
		if err := s.o.Provider.ChangePlan(ctx, a.SubscriptionID, plan); err != nil {
			return nil, err
		}
		if err := s.o.Store.Audit(ctx, by, AuditChangePlan, org, map[string]string{"plan": planID}); err != nil {
			s.log.Errorf("billing: %v", err)
		}
		return &CheckoutResult{PlanChanged: true}, nil
	}
	// only the newest checkout page can be completed
	if err := s.o.Provider.ExpireOpenCheckouts(ctx, a.CustomerID); err != nil {
		return nil, err
	}
	u, err := s.o.Provider.CheckoutURL(ctx, CheckoutRequest{
		Organization: org, CustomerID: a.CustomerID, Plan: plan,
		SuccessURL: s.returnURL("checkout=success"), CancelURL: s.returnURL("checkout=canceled"),
		PeriodStart: MonthStartOf(s.now()).AddDate(0, 1, 0),
		FreeUntil:   freeUntil(b, s.now()),
	})
	if err != nil {
		return nil, err
	}
	if err := s.o.Store.Audit(ctx, by, AuditCheckout, org, map[string]string{"plan": planID}); err != nil {
		s.log.Errorf("billing: %v", err)
	}
	return &CheckoutResult{URL: u}, nil
}

// freeUntil is when charging starts for an organization in a trial or comped (nil: now).
func freeUntil(b *Billing, now time.Time) *time.Time {
	var t time.Time
	switch {
	case b.Status == StatusTrial && b.TrialEndsAt != nil:
		t = *b.TrialEndsAt
	case b.Status == StatusComped && b.CompedUntil != nil:
		t = DayStart(*b.CompedUntil).AddDate(0, 0, 1)
	default:
		return nil
	}
	if !t.After(now) {
		return nil
	}
	return &t
}

// live reports whether a subscription still bills.
func live(st SubscriptionStatus) bool {
	switch st {
	case SubscriptionActive, SubscriptionTrialing, SubscriptionPastDue, SubscriptionUnpaid, SubscriptionPaused:
		return true
	}
	return false
}

// MonthStartOf is the start of t's UTC month.
func MonthStartOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// PortalURL returns the provider's customer portal for org (payment methods, invoices, tax ids, cancellation).
// It works while read-only: it is how an overdue invoice gets paid.
func (s *Service) PortalURL(ctx context.Context, by Actor, org, orgName string) (string, error) {
	email := ""
	if strings.Contains(by.Username, "@") {
		email = by.Username
	}
	a, _, err := s.ensureCustomer(ctx, org, orgName, email)
	if err != nil {
		return "", err
	}
	return s.o.Provider.PortalURL(ctx, a.CustomerID, s.o.ReturnURL)
}

// Invoices lists org's invoices (none without a customer).
func (s *Service) Invoices(ctx context.Context, org string, limit int) ([]Invoice, error) {
	a, err := s.o.Store.Account(ctx, org)
	if err != nil {
		return nil, err
	}
	if a == nil || a.CustomerID == "" {
		return []Invoice{}, nil
	}
	return s.o.Provider.Invoices(ctx, a.CustomerID, limit)
}

// ValidReturnURL checks a configured return URL: absolute http(s).
func ValidReturnURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("billing: return_url %q must be an absolute http(s) URL", raw)
	}
	return nil
}
