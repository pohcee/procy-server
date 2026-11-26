package billing_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/api"
	"github.com/pohcee/procy-server/server/billing"
	"github.com/pohcee/procy-server/server/billing/stripe/stripetest"
	"github.com/pohcee/procy-server/server/cluster"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/tenant"
)

func newHTTP(e *env) *billing.HTTP {
	return billing.NewHTTP(billing.HTTPOptions{
		Service:     e.svc,
		CurrentUser: func(context.Context) (string, error) { return "admin@acme.test", nil },
		Respond:     &api.Responder{Log: logger.NewLogger("t", logger.LogOutput{}, logger.LogLevelError)},
	})
}

func TestWebhookRoute(t *testing.T) {
	e := newEnv(t)
	e.link("acme", "cus_1")
	h := newHTTP(e)
	post := func(body []byte, sig string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/billing/webhook", bytes.NewReader(body))
		req.Header.Set("Stripe-Signature", sig)
		rec := httptest.NewRecorder()
		h.Webhook(rec, req)
		return rec.Code
	}
	body, sig := stripetest.SignedEvent("evt_1", "invoice.payment_failed", invoice("in_1", "cus_1"), stripetest.WebhookSecret, time.Now())
	assert.Equal(t, http.StatusOK, post(body, sig))
	assert.Equal(t, http.StatusOK, post(body, sig), "a repeated delivery is acknowledged")
	assert.Equal(t, http.StatusBadRequest, post(body, "t=1,v1=00"))
	assert.Equal(t, http.StatusBadRequest, post(bytes.Repeat([]byte("x"), 2<<20), sig), "oversized bodies are refused")
}

func TestOrgRoutes(t *testing.T) {
	e := newEnv(t)
	h := newHTTP(e)
	r := mux.NewRouter()
	h.MountOrg(r.PathPrefix("/billing").Subrouter())
	do := func(method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(tenant.WithID(req.Context(), "acme"))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	code, out := do(http.MethodPost, "/billing/checkout", `{"plan":"starter"}`)
	require.Equal(t, http.StatusOK, code, out)
	assert.Contains(t, out["data"].(map[string]any)["url"], "checkout.stripe.test")
	code, _ = do(http.MethodPost, "/billing/checkout", `{"plan":"free"}`)
	assert.Equal(t, http.StatusConflict, code)
	code, _ = do(http.MethodPost, "/billing/checkout", `{"plan":"starter","extra":1}`)
	assert.Equal(t, http.StatusBadRequest, code)
	code, out = do(http.MethodPost, "/billing/portal", ``)
	require.Equal(t, http.StatusOK, code, out)
	assert.Contains(t, out["data"].(map[string]any)["url"], "billing.stripe.test")
	code, out = do(http.MethodGet, "/billing/payment", ``)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "stripe", out["data"].(map[string]any)["provider"])
	assert.Equal(t, "full", out["data"].(map[string]any)["access"])
	code, _ = do(http.MethodGet, "/billing/invoices?limit=5", ``)
	assert.Equal(t, http.StatusOK, code)
	code, _ = do(http.MethodGet, "/billing/invoices?limit=500", ``)
	assert.Equal(t, http.StatusBadRequest, code)
}

func TestReadOnlyGuard(t *testing.T) {
	e := newEnv(t)
	e.link("acme", "cus_1")
	e.link("beta", "cus_2")
	failed := e.now.AddDate(0, 0, -15)
	a := e.store.Accts["acme"]
	a.Dunning = billing.Dunning{State: billing.DunningReadOnly, FailedAt: &failed}
	e.store.Accts["acme"] = a
	resp := &api.Responder{Log: logger.NewLogger("t", logger.LogOutput{}, logger.LogLevelError)}
	guard := billing.ReadOnlyGuard(e.svc, resp, "/api/v1")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	do := func(org, method, path string, ws bool) int {
		req := httptest.NewRequest(method, path, nil)
		if ws {
			req.Header.Set("Upgrade", "websocket")
		}
		req = req.WithContext(tenant.WithID(req.Context(), tenant.ID(org)))
		rec := httptest.NewRecorder()
		guard.ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden {
			assert.Contains(t, rec.Body.String(), billing.ReadOnlyErrCode)
		}
		return rec.Code
	}
	assert.Equal(t, http.StatusTeapot, do("acme", http.MethodGet, "/api/v1/clients", false), "reading stays possible")
	assert.Equal(t, http.StatusForbidden, do("acme", http.MethodPost, "/api/v1/commands", false))
	assert.Equal(t, http.StatusForbidden, do("acme", http.MethodPut, "/api/v1/clients/c1/tunnels", false))
	assert.Equal(t, http.StatusForbidden, do("acme", http.MethodGet, "/api/v1/ws/commands", true), "websockets start jobs")
	assert.Equal(t, http.StatusTeapot, do("acme", http.MethodPost, "/api/v1/billing/portal", false), "paying stays possible")
	assert.Equal(t, http.StatusForbidden, do("acme", http.MethodPut, "/api/v1/billing/spending-limit", false))
	assert.Equal(t, http.StatusForbidden, do("acme", http.MethodPost, "/api/v1/members", false))
	assert.Equal(t, http.StatusTeapot, do("acme", http.MethodDelete, "/api/v1/logout", false))
	assert.Equal(t, http.StatusTeapot, do("acme", http.MethodPost, "/api/v1/login", false))
	assert.Equal(t, http.StatusTeapot, do("beta", http.MethodPost, "/api/v1/commands", false), "other organizations are not affected")
	assert.Equal(t, http.StatusTeapot, do("_unresolved", http.MethodPost, "/api/v1/billing/webhook", false))
}

func TestSchedulesPauseWhileReadOnly(t *testing.T) {
	e := newEnv(t)
	e.link("acme", "cus_1")
	c := billing.PauseSchedules(cluster.Standalone{}, e.svc)
	ok, err := c.ClaimRun(tenant.WithID(ctx, "acme"), "s1", time.Now())
	require.NoError(t, err)
	assert.True(t, ok)

	failed := e.now.AddDate(0, 0, -15)
	a := e.store.Accts["acme"]
	a.Dunning = billing.Dunning{State: billing.DunningReadOnly, FailedAt: &failed}
	e.store.Accts["acme"] = a
	*e.now = e.now.Add(time.Minute) // past the access cache
	ok, err = c.ClaimRun(tenant.WithID(ctx, "acme"), "s1", time.Now())
	require.NoError(t, err)
	assert.False(t, ok)
}
