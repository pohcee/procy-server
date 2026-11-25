package chserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/billing"
	chshare "github.com/openrport/openrport/share"
	"github.com/openrport/openrport/share/tenant"
)

// ErrCodeTransferLimit is the error code of a tunnel or upload refused at the hard transfer cap.
const ErrCodeTransferLimit = "ERR_CODE_TRANSFER_LIMIT"

// BillingAPI is the billing of the API ([billing] enabled): an organization's administrators read their account
// and periods and set their spending limit; Enforcer refuses new tunnels and uploads at the hard cap.
type BillingAPI struct {
	Org      billing.OrgService
	Operator billing.OperatorService
	Enforcer *billing.Enforcer
	// Provider is the payment provider (nil without one).
	Provider *billing.Service
}

// billingResponse is GET /billing: the account, the plan, the policy of the current period.
type billingResponse struct {
	*billing.Account
}

func (al *APIListener) billingFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, billing.ErrInvalid):
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, billing.ErrNotFound):
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, err.Error())
	default:
		al.jsonError(w, err)
	}
}

// handleGetBilling handles GET /billing: the organization's plan, billing status, allowance and transfer state.
func (al *APIListener) handleGetBilling(w http.ResponseWriter, req *http.Request) {
	tid, err := tenant.FromContext(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}
	a, err := al.billing.Org.Account(req.Context(), tid)
	if err != nil {
		al.billingFail(w, err)
		return
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(billingResponse{a}))
}

// handleGetBillingPeriods handles GET /billing/periods: the closed periods, newest first.
func (al *APIListener) handleGetBillingPeriods(w http.ResponseWriter, req *http.Request) {
	tid, err := tenant.FromContext(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}
	ps, err := al.billing.Org.Periods(req.Context(), tid)
	if err != nil {
		al.billingFail(w, err)
		return
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(ps))
}

// spendingLimitRequest needs the field present: {"spending_limit": null} removes the limit.
type spendingLimitRequest struct {
	SpendingLimit json.RawMessage `json:"spending_limit"`
}

// handlePutSpendingLimit handles PUT /billing/spending-limit {"spending_limit": USD or null}.
func (al *APIListener) handlePutSpendingLimit(w http.ResponseWriter, req *http.Request) {
	tid, err := tenant.FromContext(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}
	var in spendingLimitRequest
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || len(in.SpendingLimit) == 0 {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "spending_limit is required (USD per month, or null for no limit)")
		return
	}
	var usd *float64
	if err := json.Unmarshal(in.SpendingLimit, &usd); err != nil || (usd != nil && (*usd < 0 || *usd > 1e9 || math.IsNaN(*usd))) {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "spending_limit must be an amount in USD between 0 and 1000000000, or null")
		return
	}
	var cents *int64
	if usd != nil {
		c := billing.Cents(*usd)
		cents = &c
	}
	user, err := al.getUserModelForAuth(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}
	a, err := al.billing.Org.SetSpendingLimit(req.Context(), tid, billing.Actor{Username: user.Username, RemoteIP: chshare.RemoteIP(req)}, cents)
	if err != nil {
		al.billingFail(w, err)
		return
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(billingResponse{a}))
}

// transferGuard refuses the request with 403 ERR_CODE_TRANSFER_LIMIT while the request's organization is at its
// hard transfer cap (new tunnels and uploads). Nothing else is limited.
func (al *APIListener) transferGuard(next http.Handler) http.Handler {
	if al.billing == nil || al.billing.Enforcer == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if tid, err := tenant.FromContext(req.Context()); err == nil {
			if err := al.billing.Enforcer.Allow(tid); err != nil {
				al.jsonErrorResponseWithErrCode(w, http.StatusForbidden, ErrCodeTransferLimit, fmt.Sprint(err))
				return
			}
		}
		next.ServeHTTP(w, req)
	})
}
