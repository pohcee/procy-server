package stripe

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	sg "github.com/stripe/stripe-go/v87"
	"github.com/stripe/stripe-go/v87/webhook"

	"github.com/pohcee/procy-server/server/billing"
)

// ParseWebhook implements billing.Provider: the Stripe-Signature header is verified over the raw body with the
// endpoint secret (5 minutes' tolerance) before anything is parsed; then the event is normalized. Signature
// failures wrap billing.ErrSignature; an event of another API release train, or a body that doesn't parse, is an
// error too (Stripe retries it; fix the endpoint's API version).
func (p *Provider) ParseWebhook(body []byte, header http.Header) (*billing.Event, error) {
	e, err := webhook.ConstructEventWithOptions(body, header.Get("Stripe-Signature"), p.webhookSecret,
		webhook.ConstructEventOptions{Tolerance: WebhookTolerance})
	if err != nil {
		if isSignatureError(err) {
			return nil, fmt.Errorf("%w: %v", billing.ErrSignature, err)
		}
		return nil, fmt.Errorf("stripe: webhook: %w", err)
	}
	if e.ID == "" {
		return nil, errors.New("stripe: webhook: event without id")
	}
	out := &billing.Event{ID: e.ID, ProviderType: string(e.Type), Created: time.Unix(e.Created, 0).UTC(), Type: billing.EventIgnored}
	if e.Data == nil {
		return out, nil
	}
	raw := e.Data.Raw
	switch e.Type {
	case sg.EventTypeCheckoutSessionCompleted:
		var s sg.CheckoutSession
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("stripe: webhook %s: %w", e.Type, err)
		}
		if s.Mode != sg.CheckoutSessionModeSubscription {
			return out, nil
		}
		out.Type = billing.EventCheckoutCompleted
		out.Organization = s.ClientReferenceID
		if out.Organization == "" {
			out.Organization = s.Metadata[OrgMetadataKey]
		}
		if s.Customer != nil {
			out.CustomerID = s.Customer.ID
		}
		if s.Subscription != nil {
			out.SubscriptionID = s.Subscription.ID
			out.SubscriptionStatus = billing.SubscriptionStatus(s.Subscription.Status)
		}
		out.PaymentMethod = s.Status == sg.CheckoutSessionStatusComplete
	case sg.EventTypeCustomerSubscriptionCreated, sg.EventTypeCustomerSubscriptionUpdated,
		sg.EventTypeCustomerSubscriptionDeleted, sg.EventTypeCustomerSubscriptionTrialWillEnd:
		var s sg.Subscription
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("stripe: webhook %s: %w", e.Type, err)
		}
		switch e.Type {
		case sg.EventTypeCustomerSubscriptionDeleted:
			out.Type = billing.EventSubscriptionCanceled
		case sg.EventTypeCustomerSubscriptionTrialWillEnd:
			out.Type = billing.EventTrialWillEnd
		default:
			out.Type = billing.EventSubscriptionChanged
		}
		sub := subscriptionOf(&s)
		out.CustomerID = sub.CustomerID
		out.SubscriptionID = sub.ID
		out.SubscriptionStatus = sub.Status
		out.PriceIDs = sub.PriceIDs
		out.Organization = s.Metadata[OrgMetadataKey]
	case sg.EventTypeInvoicePaid, sg.EventTypeInvoicePaymentFailed:
		var inv sg.Invoice
		if err := json.Unmarshal(raw, &inv); err != nil {
			return nil, fmt.Errorf("stripe: webhook %s: %w", e.Type, err)
		}
		out.Type = billing.EventPaymentFailed
		if e.Type == sg.EventTypeInvoicePaid {
			out.Type = billing.EventInvoicePaid
			out.PaymentMethod = inv.AmountPaid > 0
		}
		out.InvoiceID = inv.ID
		if inv.Customer != nil {
			out.CustomerID = inv.Customer.ID
		}
		if inv.Parent != nil && inv.Parent.SubscriptionDetails != nil {
			if inv.Parent.SubscriptionDetails.Subscription != nil {
				out.SubscriptionID = inv.Parent.SubscriptionDetails.Subscription.ID
			}
			out.Organization = inv.Parent.SubscriptionDetails.Metadata[OrgMetadataKey]
		}
	}
	return out, nil
}

func isSignatureError(err error) bool {
	for _, target := range []error{webhook.ErrInvalidHeader, webhook.ErrNoValidSignature, webhook.ErrNotSigned, webhook.ErrTooOld, webhook.ErrEmptySecret} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
