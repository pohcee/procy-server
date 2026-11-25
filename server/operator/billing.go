package operator

import (
	"net/http"
	"time"

	"github.com/gorilla/mux"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/billing"
)

// mountBilling registers the billing routes ([billing] enabled).
func (h *Handler) mountBilling(r *mux.Router) {
	r.HandleFunc("/billing", h.billingAccounts).Methods(http.MethodGet)
	if h.plans != nil {
		r.HandleFunc("/billing/plans", h.billingPlans).Methods(http.MethodGet)
	}
	r.HandleFunc("/billing/periods", h.billingPeriods).Methods(http.MethodGet)
	r.HandleFunc("/organizations/{id}/billing", h.billingAccount).Methods(http.MethodGet)
	r.HandleFunc("/organizations/{id}/billing/plan", h.billingSetPlan).Methods(http.MethodPut)
	r.HandleFunc("/organizations/{id}/billing/status", h.billingSetStatus).Methods(http.MethodPut)
	r.HandleFunc("/organizations/{id}/billing/override", h.billingSetOverride).Methods(http.MethodPut)
	r.HandleFunc("/organizations/{id}/billing/periods", h.billingPeriods).Methods(http.MethodGet)
	r.HandleFunc("/organizations/{id}/billing/periods/{month}/approve", h.billingApprove).Methods(http.MethodPost)
}

func billingActor(r *http.Request) billing.Actor {
	a := actor(r)
	return billing.Actor{Username: a.Username, RemoteIP: a.RemoteIP}
}

func (h *Handler) billingAccounts(w http.ResponseWriter, r *http.Request) {
	out, err := h.billing.Accounts(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	if out == nil {
		out = []billing.Account{}
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(out))
}

func (h *Handler) billingPlans(w http.ResponseWriter, _ *http.Request) {
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(h.plans.Catalog()))
}

func (h *Handler) billingAccount(w http.ResponseWriter, r *http.Request) {
	a, err := h.billing.Account(r.Context(), mux.Vars(r)["id"])
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(a))
}

type planRequest struct {
	Plan string `json:"plan"`
}

func (h *Handler) billingSetPlan(w http.ResponseWriter, r *http.Request) {
	var in planRequest
	if !h.decode(w, r, &in) {
		return
	}
	a, err := h.billing.SetPlan(r.Context(), billingActor(r), mux.Vars(r)["id"], in.Plan)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(a))
}

func (h *Handler) billingSetStatus(w http.ResponseWriter, r *http.Request) {
	var in billing.StatusChange
	if !h.decode(w, r, &in) {
		return
	}
	a, err := h.billing.SetStatus(r.Context(), billingActor(r), mux.Vars(r)["id"], in)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(a))
}

func (h *Handler) billingSetOverride(w http.ResponseWriter, r *http.Request) {
	var in billing.Override
	if !h.decode(w, r, &in) {
		return
	}
	a, err := h.billing.SetOverride(r.Context(), billingActor(r), mux.Vars(r)["id"], in)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(a))
}

// billingPeriods answers GET /billing/periods?state= (every organization) and
// /organizations/{id}/billing/periods?state=.
func (h *Handler) billingPeriods(w http.ResponseWriter, r *http.Request) {
	state := billing.PeriodState(r.URL.Query().Get("state"))
	switch state {
	case "", billing.PeriodReady, billing.PeriodHeld, billing.PeriodNotCharged, billing.PeriodNeverCharged, billing.PeriodReported:
	default:
		h.respond.ErrorWithTitle(w, http.StatusBadRequest, "state must be ready, held, not_charged, never_charged or reported")
		return
	}
	out, err := h.billing.Periods(r.Context(), mux.Vars(r)["id"], state)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(out))
}

func (h *Handler) billingApprove(w http.ResponseWriter, r *http.Request) {
	month, err := time.Parse("2006-01", mux.Vars(r)["month"])
	if err != nil {
		h.respond.ErrorWithTitle(w, http.StatusBadRequest, "the period is a month, YYYY-MM")
		return
	}
	id := mux.Vars(r)["id"]
	if err := h.billing.ApprovePeriod(r.Context(), billingActor(r), id, month); err != nil {
		h.fail(w, err)
		return
	}
	out, err := h.billing.Periods(r.Context(), id, "")
	if err != nil {
		h.fail(w, err)
		return
	}
	for _, p := range out {
		if p.Start.UTC().Equal(month) {
			h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(p))
			return
		}
	}
	h.fail(w, billing.ErrNotFound)
}
