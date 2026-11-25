package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/openrport/openrport/server/api"
	chshare "github.com/openrport/openrport/share"
	"github.com/openrport/openrport/share/tenant"
)

// Routes of the payment provider integration, below the API prefix (/api/v1).
const (
	// WebhookRoute takes the provider's webhooks: no session authentication, signature-verified.
	WebhookRoute = "/billing/webhook"
	// OrgRoutePrefix holds the organization administrators' payment routes (behind authentication and the
	// Administrators check).
	OrgRoutePrefix = "/billing"
	// OperatorRoutePrefix holds the operators' payment provider routes, below the operator API (/operator).
	OperatorRoutePrefix = "/billing/provider"
)

// MaxWebhookBody bounds a webhook body (Stripe events are a few KiB, up to a few hundred with large objects).
const MaxWebhookBody = 1 << 20

// WebhookRouteName names the webhook route, so the API's request size limit gives it MaxWebhookBody.
const WebhookRouteName = "billing-webhook"

// HTTPOptions are the dependencies of the HTTP handlers.
type HTTPOptions struct {
	Service *Service
	// CurrentUser returns the authenticated user's name (an errors.APIError when there is none).
	CurrentUser func(ctx context.Context) (string, error)
	// OrgName returns an organization's display name (optional; the id otherwise).
	OrgName func(ctx context.Context, org string) string
	Respond *api.Responder
}

// HTTP serves the payment routes.
type HTTP struct {
	o HTTPOptions
}

// NewHTTP returns the handlers.
func NewHTTP(o HTTPOptions) *HTTP { return &HTTP{o: o} }

// Webhook handles POST /billing/webhook: the body is read raw (bounded), verified, its event claimed and applied
// in one transaction. 200 for an applied or repeated event, 400 for a request that isn't from the provider (counted
// for operators), 500 when applying failed (the provider retries).
func (h *HTTP) Webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxWebhookBody+1))
	if err != nil || len(body) > MaxWebhookBody {
		h.o.Service.count(func(st *Stats) { st.WebhooksUnverified++ })
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	res, err := h.o.Service.HandleWebhook(r.Context(), body, r.Header)
	switch res {
	case WebhookApplied, WebhookDuplicate:
		w.WriteHeader(http.StatusOK)
	case WebhookUnverified:
		h.o.Service.log.Infof("billing: webhook from %s refused: %v", remoteIP(r), err)
		http.Error(w, "bad request", http.StatusBadRequest)
	case WebhookUnreadable:
		h.o.Service.log.Errorf("billing: webhook: a verified event can't be read (check the endpoint's API version): %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
	default:
		h.o.Service.log.Errorf("billing: webhook: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// MountOrg registers the organization administrators' routes on r, a router for OrgRoutePrefix behind
// authentication and the Administrators check.
func (h *HTTP) MountOrg(r *mux.Router) {
	r.HandleFunc("/payment", h.payment).Methods(http.MethodGet)
	r.HandleFunc("/invoices", h.invoices).Methods(http.MethodGet)
	r.HandleFunc("/portal", h.portal).Methods(http.MethodPost)
	r.HandleFunc("/checkout", h.checkout).Methods(http.MethodPost)
}

// MountOperator registers the operators' routes on r, a router for OperatorRoutePrefix inside the operator API
// (which checks the operator).
func (h *HTTP) MountOperator(r *mux.Router) {
	r.HandleFunc("/accounts", h.accounts).Methods(http.MethodGet)
	r.HandleFunc("/reports", h.reports).Methods(http.MethodGet)
	r.HandleFunc("/stats", h.stats).Methods(http.MethodGet)
	r.HandleFunc("/accounts/{id}/restore", h.restore).Methods(http.MethodPost)
	r.HandleFunc("/accounts/{id}/confirm-suspension", h.confirmSuspension).Methods(http.MethodPost)
}

func (h *HTTP) org(w http.ResponseWriter, r *http.Request) (string, bool) {
	tid, err := tenant.FromContext(r.Context())
	if err != nil {
		h.o.Respond.Error(w, err)
		return "", false
	}
	return string(tid), true
}

func (h *HTTP) actor(w http.ResponseWriter, r *http.Request) (Actor, bool) {
	name, err := h.o.CurrentUser(r.Context())
	if err != nil {
		h.o.Respond.Error(w, err)
		return Actor{}, false
	}
	return Actor{Username: name, RemoteIP: remoteIP(r)}, true
}

// remoteIP is the client's address: X-Forwarded-For from a trusted proxy ([api] trusted_proxies), else the peer.
func remoteIP(r *http.Request) string {
	return chshare.RemoteIP(r)
}

func (h *HTTP) orgName(ctx context.Context, org string) string {
	if h.o.OrgName != nil {
		if n := h.o.OrgName(ctx, org); n != "" {
			return n
		}
	}
	return org
}

func (h *HTTP) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		h.o.Respond.ErrorWithTitle(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrConflict):
		h.o.Respond.ErrorWithTitle(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrReadOnly):
		h.o.Respond.ErrorWithErrCode(w, http.StatusForbidden, ReadOnlyErrCode, ErrReadOnly.Error())
	case errors.Is(err, ErrNoProvider):
		h.o.Respond.ErrorWithTitle(w, http.StatusNotImplemented, err.Error())
	default:
		h.o.Service.log.Errorf("billing: %v", err)
		h.o.Respond.ErrorWithTitle(w, http.StatusBadGateway, "the payment provider could not be reached; try again later")
	}
}

func (h *HTTP) payment(w http.ResponseWriter, r *http.Request) {
	org, ok := h.org(w, r)
	if !ok {
		return
	}
	v, err := h.o.Service.Payment(r.Context(), org)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.o.Respond.JSON(w, http.StatusOK, api.NewSuccessPayload(v))
}

func (h *HTTP) invoices(w http.ResponseWriter, r *http.Request) {
	org, ok := h.org(w, r)
	if !ok {
		return
	}
	limit := 24
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 100 {
			h.o.Respond.ErrorWithTitle(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return
		}
		limit = n
	}
	inv, err := h.o.Service.Invoices(r.Context(), org, limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.o.Respond.JSON(w, http.StatusOK, api.NewSuccessPayload(inv))
}

func (h *HTTP) portal(w http.ResponseWriter, r *http.Request) {
	org, ok := h.org(w, r)
	if !ok {
		return
	}
	by, ok := h.actor(w, r)
	if !ok {
		return
	}
	u, err := h.o.Service.PortalURL(r.Context(), by, org, h.orgName(r.Context(), org))
	if err != nil {
		h.fail(w, err)
		return
	}
	h.o.Respond.JSON(w, http.StatusOK, api.NewSuccessPayload(map[string]string{"url": u}))
}

func (h *HTTP) checkout(w http.ResponseWriter, r *http.Request) {
	org, ok := h.org(w, r)
	if !ok {
		return
	}
	by, ok := h.actor(w, r)
	if !ok {
		return
	}
	var in struct {
		Plan string `json:"plan"`
	}
	if !decodeJSON(w, r, h.o.Respond, &in) {
		return
	}
	if in.Plan == "" {
		h.o.Respond.ErrorWithTitle(w, http.StatusBadRequest, "plan is required")
		return
	}
	res, err := h.o.Service.Checkout(r.Context(), by, org, h.orgName(r.Context(), org), in.Plan)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.o.Respond.JSON(w, http.StatusOK, api.NewSuccessPayload(res))
}

func jsonDecoder(r io.Reader) *json.Decoder {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	return dec
}

func decodeJSON(w http.ResponseWriter, r *http.Request, resp *api.Responder, v any) bool {
	dec := jsonDecoder(io.LimitReader(r.Body, 64<<10))
	if err := dec.Decode(v); err != nil {
		resp.ErrorWithError(w, http.StatusBadRequest, "invalid JSON body", err)
		return false
	}
	return true
}

func (h *HTTP) accounts(w http.ResponseWriter, r *http.Request) {
	v, err := h.o.Service.Accounts(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	h.o.Respond.JSON(w, http.StatusOK, api.NewSuccessPayload(v))
}

func (h *HTTP) reports(w http.ResponseWriter, r *http.Request) {
	var start time.Time
	if s := r.URL.Query().Get("period"); s != "" {
		t, err := time.Parse("2006-01", s)
		if err != nil {
			h.o.Respond.ErrorWithTitle(w, http.StatusBadRequest, "period must be a month: YYYY-MM")
			return
		}
		start = t.UTC()
	}
	v, err := h.o.Service.Reports(r.Context(), start)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.o.Respond.JSON(w, http.StatusOK, api.NewSuccessPayload(v))
}

func (h *HTTP) stats(w http.ResponseWriter, _ *http.Request) {
	h.o.Respond.JSON(w, http.StatusOK, api.NewSuccessPayload(h.o.Service.Stats()))
}

func (h *HTTP) restore(w http.ResponseWriter, r *http.Request) {
	by, ok := h.actor(w, r)
	if !ok {
		return
	}
	a, err := h.o.Service.Restore(r.Context(), by, mux.Vars(r)["id"])
	if err != nil {
		h.fail(w, err)
		return
	}
	h.o.Respond.JSON(w, http.StatusOK, api.NewSuccessPayload(a))
}

func (h *HTTP) confirmSuspension(w http.ResponseWriter, r *http.Request) {
	by, ok := h.actor(w, r)
	if !ok {
		return
	}
	if err := h.o.Service.ConfirmSuspension(r.Context(), by, mux.Vars(r)["id"]); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ReadOnlyErrCode is the error code of requests refused while an organization is read-only.
const ReadOnlyErrCode = "ERR_CODE_BILLING_READ_ONLY"

// readOnlyAllowed are the changes a read-only organization's users may still make: their own session and
// account, and the portal (paying is how read-only ends; checkout answers for itself that it is refused).
var readOnlyAllowed = []string{"/login", "/logout", "/me", "/verify-2fa", OrgRoutePrefix + "/portal", OrgRoutePrefix + "/checkout"}

// ReadOnlyGuard refuses changes (any method but GET, HEAD and OPTIONS; and websocket upgrades, which start jobs
// and uploads) for organizations that are read-only after an overdue payment, with 403 and ReadOnlyErrCode.
// prefix is the API prefix stripped before matching readOnlyAllowed. Reading stays possible; devices, tunnels
// already open and the provider's portal are not affected.
func ReadOnlyGuard(svc *Service, resp *api.Responder, prefix string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !mutating(r) || allowedWhileReadOnly(strings.TrimPrefix(r.URL.Path, prefix)) {
				next.ServeHTTP(w, r)
				return
			}
			tid, err := tenant.FromContext(r.Context())
			if err != nil || strings.HasPrefix(string(tid), "_") || svc.Access(r.Context(), string(tid)) != AccessReadOnly {
				next.ServeHTTP(w, r)
				return
			}
			resp.ErrorWithErrCode(w, http.StatusForbidden, ReadOnlyErrCode, ErrReadOnly.Error())
		})
	}
}

func mutating(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
	}
	return true
}

func allowedWhileReadOnly(path string) bool {
	for _, p := range readOnlyAllowed {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}
