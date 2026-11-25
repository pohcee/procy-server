// Package stripetest is a fake Stripe API on httptest for the tests of the Stripe provider and of billing: the few
// endpoints the provider calls, with Stripe's request encoding (form values) and response shapes. No real Stripe
// calls, no keys.
package stripetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v87/webhook"
)

// SecretKey and WebhookSecret are the test credentials the fake accepts.
const (
	SecretKey     = "sk_test_fake"
	WebhookSecret = "whsec_test_fake" //nolint:gosec // a test secret, not a credential
)

// MeterEvent is a meter event the fake received.
type MeterEvent struct {
	EventName  string
	Identifier string
	Customer   string
	Value      int64
	Timestamp  time.Time
}

// Fake is the fake Stripe API.
type Fake struct {
	Server *httptest.Server

	mu sync.Mutex
	// Customers by id, with their metadata.
	Customers map[string]map[string]string
	// Deleted customers.
	Deleted map[string]bool
	// Prices: price id → meter id (metered) or "" (licensed).
	Prices map[string]string
	// Meters: meter id → event name.
	Meters map[string]string
	// MeterEvents received, in order (duplicates of an identifier are refused and not added).
	MeterEvents []MeterEvent
	// Subscriptions: id → status, customer and items.
	Subscriptions map[string]*Subscription
	// Invoices of each customer, as JSON objects.
	Invoices map[string][]map[string]any
	// Requests are the paths requested ("POST /v1/customers").
	Requests []string
	// Forms are the form bodies of the POST requests, by path.
	Forms map[string][]url.Values
	// FailMeterEvents makes the meter events endpoint answer 500.
	FailMeterEvents bool
	// SummaryLag hides meter events received less than this long ago from the summaries.
	SummaryLag time.Duration

	seq int
}

// Subscription is a fake subscription.
type Subscription struct {
	ID       string
	Customer string
	Status   string
	Items    []string // price ids
	Metadata map[string]string
	Cancel   bool
}

// New starts the fake; it stops with the test.
func New(t testing.TB) *Fake {
	f := &Fake{
		Customers:     map[string]map[string]string{},
		Deleted:       map[string]bool{},
		Prices:        map[string]string{},
		Meters:        map[string]string{},
		Subscriptions: map[string]*Subscription{},
		Invoices:      map[string][]map[string]any{},
		Forms:         map[string][]url.Values{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}

// AddMeteredPrice adds a metered price backed by a new meter with event name eventName.
func (f *Fake) AddMeteredPrice(priceID, eventName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	meterID := "mtr_" + eventName
	f.Prices[priceID] = meterID
	f.Meters[meterID] = eventName
}

// AddLicensedPrice adds a licensed (in advance) price.
func (f *Fake) AddLicensedPrice(priceID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Prices[priceID] = ""
}

// Events returns a copy of the meter events received.
func (f *Fake) Events() []MeterEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]MeterEvent(nil), f.MeterEvents...)
}

// Count returns how often method path was requested.
func (f *Fake) Count(methodPath string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.Requests {
		if r == methodPath {
			n++
		}
	}
	return n
}

// LastForm returns the last form posted to path.
func (f *Fake) LastForm(path string) url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	forms := f.Forms[path]
	if len(forms) == 0 {
		return nil
	}
	return forms[len(forms)-1]
}

func (f *Fake) id(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s_%d", prefix, f.seq)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func stripeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"type": "invalid_request_error", "code": code, "message": msg}})
}

func list(data []map[string]any, path string) map[string]any {
	if data == nil {
		data = []map[string]any{}
	}
	return map[string]any{"object": "list", "data": data, "has_more": false, "url": path}
}

//nolint:gocyclo // one switch over the fake endpoints reads best
func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if user, _, ok := r.BasicAuth(); (!ok || user != SecretKey) && r.Header.Get("Authorization") != "Bearer "+SecretKey {
		stripeError(w, http.StatusUnauthorized, "", "Invalid API Key provided")
		return
	}
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, r.Method+" "+r.URL.Path)
	if r.Method == http.MethodPost {
		f.Forms[r.URL.Path] = append(f.Forms[r.URL.Path], r.PostForm)
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/customers":
		id := f.id("cus")
		meta := map[string]string{}
		for k, v := range r.PostForm {
			if strings.HasPrefix(k, "metadata[") {
				meta[strings.TrimSuffix(strings.TrimPrefix(k, "metadata["), "]")] = v[0]
			}
		}
		f.Customers[id] = meta
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "object": "customer", "metadata": meta, "email": r.PostForm.Get("email")})
	case r.Method == http.MethodGet && len(parts) == 3 && parts[1] == "customers":
		meta, ok := f.Customers[parts[2]]
		if !ok {
			stripeError(w, http.StatusNotFound, "resource_missing", "No such customer")
			return
		}
		if f.Deleted[parts[2]] {
			writeJSON(w, http.StatusOK, map[string]any{"id": parts[2], "object": "customer", "deleted": true})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": parts[2], "object": "customer", "metadata": meta})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/checkout/sessions":
		id := f.id("cs_test")
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "object": "checkout.session", "url": "https://checkout.stripe.test/c/pay/" + id})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/billing_portal/sessions":
		id := f.id("bps")
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "object": "billing_portal.session", "url": "https://billing.stripe.test/p/session/" + id})
	case r.Method == http.MethodGet && len(parts) == 3 && parts[1] == "prices":
		meterID, ok := f.Prices[parts[2]]
		if !ok {
			stripeError(w, http.StatusNotFound, "resource_missing", "No such price")
			return
		}
		price := map[string]any{"id": parts[2], "object": "price", "recurring": map[string]any{"interval": "month", "usage_type": "licensed"}}
		if meterID != "" {
			price["recurring"] = map[string]any{"interval": "month", "usage_type": "metered", "meter": meterID}
		}
		writeJSON(w, http.StatusOK, price)
	case r.Method == http.MethodGet && len(parts) == 4 && parts[1] == "billing" && parts[2] == "meters":
		name, ok := f.Meters[parts[3]]
		if !ok {
			stripeError(w, http.StatusNotFound, "resource_missing", "No such meter")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": parts[3], "object": "billing.meter", "event_name": name,
			"customer_mapping": map[string]any{"event_payload_key": "stripe_customer_id", "type": "by_id"},
			"value_settings":   map[string]any{"event_payload_key": "value"}})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/billing/meter_events":
		if f.FailMeterEvents {
			stripeError(w, http.StatusInternalServerError, "", "internal error")
			return
		}
		ident := r.PostForm.Get("identifier")
		for _, e := range f.MeterEvents {
			if e.Identifier == ident {
				stripeError(w, http.StatusBadRequest, "", "An event with identifier "+ident+" already exists.")
				return
			}
		}
		v, err := strconv.ParseInt(r.PostForm.Get("payload[value]"), 10, 64)
		ts, err2 := strconv.ParseInt(r.PostForm.Get("timestamp"), 10, 64)
		if err != nil || err2 != nil {
			stripeError(w, http.StatusBadRequest, "parameter_invalid", "bad value or timestamp")
			return
		}
		f.MeterEvents = append(f.MeterEvents, MeterEvent{EventName: r.PostForm.Get("event_name"), Identifier: ident,
			Customer: r.PostForm.Get("payload[stripe_customer_id]"), Value: v, Timestamp: time.Unix(ts, 0).UTC()})
		writeJSON(w, http.StatusOK, map[string]any{"object": "billing.meter_event", "identifier": ident, "event_name": r.PostForm.Get("event_name")})
	case r.Method == http.MethodGet && len(parts) == 5 && parts[4] == "event_summaries":
		name := f.Meters[parts[3]]
		q := r.URL.Query()
		start, _ := strconv.ParseInt(q.Get("start_time"), 10, 64)
		end, _ := strconv.ParseInt(q.Get("end_time"), 10, 64)
		if start%60 != 0 || end%60 != 0 {
			stripeError(w, http.StatusBadRequest, "parameter_invalid", "start_time and end_time must be aligned with minute boundaries")
			return
		}
		var sum int64
		for _, e := range f.MeterEvents {
			if e.EventName == name && e.Customer == q.Get("customer") && e.Timestamp.Unix() >= start && e.Timestamp.Unix() < end {
				sum += e.Value
			}
		}
		writeJSON(w, http.StatusOK, list([]map[string]any{{"id": "mtrusg_1", "object": "billing.meter_event_summary",
			"aggregated_value": sum, "start_time": start, "end_time": end, "meter": parts[3]}}, r.URL.Path))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/invoices":
		writeJSON(w, http.StatusOK, list(f.Invoices[r.URL.Query().Get("customer")], r.URL.Path))
	case len(parts) == 3 && parts[1] == "subscriptions":
		s, ok := f.Subscriptions[parts[2]]
		if !ok {
			stripeError(w, http.StatusNotFound, "resource_missing", "No such subscription")
			return
		}
		switch r.Method {
		case http.MethodPost:
			if r.PostForm.Get("cancel_at_period_end") == "true" {
				s.Cancel = true
			}
			var items []string
			changed := false
			for i := 0; ; i++ {
				k := fmt.Sprintf("items[%d]", i)
				if _, ok := r.PostForm[k+"[id]"]; !ok && r.PostForm.Get(k+"[price]") == "" {
					break
				}
				changed = true
				if p := r.PostForm.Get(k + "[price]"); p != "" {
					items = append(items, p)
				}
			}
			if changed {
				s.Items = items
			}
		case http.MethodDelete:
			s.Status = "canceled"
		}
		writeJSON(w, http.StatusOK, f.subscriptionJSON(s))
	default:
		stripeError(w, http.StatusNotFound, "resource_missing", "Unrecognized request URL: "+r.Method+" "+r.URL.Path)
	}
}

func (f *Fake) subscriptionJSON(s *Subscription) map[string]any {
	items := []map[string]any{}
	for i, p := range s.Items {
		items = append(items, map[string]any{"id": fmt.Sprintf("si_%s_%d", s.ID, i), "object": "subscription_item", "price": map[string]any{"id": p, "object": "price"}})
	}
	return map[string]any{"id": s.ID, "object": "subscription", "customer": s.Customer, "status": s.Status,
		"cancel_at_period_end": s.Cancel, "metadata": s.Metadata,
		"items": map[string]any{"object": "list", "data": items, "has_more": false}}
}

// SubscriptionJSON is the JSON of a subscription as webhooks carry it.
func (f *Fake) SubscriptionJSON(s *Subscription) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subscriptionJSON(s)
}

// APIVersion is the API version of the events SignedEvent builds: stripe-go's release train.
const APIVersion = "2026-10-28.endive"

// SignedEvent returns the body and Stripe-Signature header of a webhook event of type typ carrying object, signed
// with secret at at.
func SignedEvent(id, typ string, object any, secret string, at time.Time) ([]byte, string) {
	body, err := json.Marshal(map[string]any{
		"id": id, "object": "event", "type": typ, "api_version": APIVersion, "created": at.Unix(),
		"data": map[string]any{"object": object},
	})
	if err != nil {
		panic(err)
	}
	sp := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: secret, Timestamp: at})
	return body, sp.Header
}

// SignHeader returns the Stripe-Signature header of body signed with secret at at.
func SignHeader(body []byte, secret string, at time.Time) string {
	return webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: body, Secret: secret, Timestamp: at}).Header
}
