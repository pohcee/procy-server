package chserver

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/billing"
	"github.com/pohcee/procy-server/server/billing/billingtest"
	"github.com/pohcee/procy-server/server/billing/stripe"
	"github.com/pohcee/procy-server/server/billing/stripe/stripetest"
	"github.com/pohcee/procy-server/server/chconfig"
	"github.com/pohcee/procy-server/share/tenant"
)

type onePlan map[string]*billing.Plan

func (p onePlan) Get(id string) *billing.Plan { return p[id] }

func billingProviderListener(t *testing.T, store *billingtest.MemStore) *APIListener {
	t.Helper()
	f := stripetest.New(t)
	p, err := stripe.New(stripe.Options{SecretKey: stripetest.SecretKey, WebhookSecret: stripetest.WebhookSecret, BackendURL: f.Server.URL})
	require.NoError(t, err)
	svc, err := billing.NewService(billing.Options{Provider: p, Store: store, Plans: onePlan{}})
	require.NoError(t, err)
	al := &APIListener{
		insecureForTests: true,
		apiServices:      &apiServices{config: &chconfig.Config{API: chconfig.APIConfig{MaxRequestBytes: 10 * 1024}}},
		Logger:           testLog,
	}
	al.billing = &BillingAPI{Provider: svc}
	al.initRouter()
	return al
}

// Without a payment provider (standalone, provider "none") there is no webhook route.
func TestNoBillingWebhookWithoutProvider(t *testing.T) {
	al := &APIListener{
		insecureForTests: true,
		apiServices:      &apiServices{config: &chconfig.Config{API: chconfig.APIConfig{MaxRequestBytes: 1024 * 1024}}},
		Logger:           testLog,
	}
	al.initRouter()
	for _, path := range []string{"/api/v1/billing/webhook", "/api/v1/billing/checkout", "/api/v1/billing/portal"} {
		w := httptest.NewRecorder()
		al.router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		assert.Equal(t, http.StatusNotFound, w.Code, path)
	}
}

func TestBillingWebhookRouteNeedsNoSessionButASignature(t *testing.T) {
	store := billingtest.NewMemStore()
	store.Accts["acme"] = billing.ProviderAccount{Organization: "acme", Provider: "stripe", CustomerID: "cus_1"}
	al := billingProviderListener(t, store)
	// a real invoice event is often larger than API requests may be (10 KB by default)
	lines := make([]map[string]any, 200)
	for i := range lines {
		lines[i] = map[string]any{"id": fmt.Sprintf("il_%d", i), "object": "line_item", "description": "a line of an invoice with tax"}
	}
	body, sig := stripetest.SignedEvent("evt_1", "invoice.payment_failed",
		map[string]any{"id": "in_1", "object": "invoice", "customer": "cus_1", "lines": map[string]any{"object": "list", "data": lines}},
		stripetest.WebhookSecret, time.Now())
	require.Greater(t, len(body), 10*1024)
	post := func(sig string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/billing/webhook", bytes.NewReader(body))
		req.Header.Set("Stripe-Signature", sig)
		w := httptest.NewRecorder()
		al.router.ServeHTTP(w, req)
		return w.Code
	}
	assert.Equal(t, http.StatusOK, post(sig))
	assert.Equal(t, billing.DunningPastDue, store.Accts["acme"].State)
	assert.Equal(t, http.StatusOK, post(sig), "a replay is acknowledged and ignored")
	assert.Equal(t, http.StatusBadRequest, post("t=1,v1=deadbeef"))
}

func TestReadOnlyOrganizationsCantChangeAnything(t *testing.T) {
	store := billingtest.NewMemStore()
	failed := time.Now().AddDate(0, 0, -20)
	store.Accts[string(tenant.Default)] = billing.ProviderAccount{Organization: string(tenant.Default), Provider: "stripe", CustomerID: "cus_1",
		Dunning: billing.Dunning{State: billing.DunningReadOnly, FailedAt: &failed}}
	al := billingProviderListener(t, store)
	w := httptest.NewRecorder()
	al.router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/v1/commands", bytes.NewReader([]byte(`{}`))))
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), billing.ReadOnlyErrCode)
	assert.Contains(t, w.Body.String(), "payment overdue")
}
