package chserver

import (
	"context"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/openrport/openrport/server/billing"
	"github.com/openrport/openrport/server/routes"
)

// billingProvider is the payment provider of the API; nil without one (standalone, [billing] provider "none").
func (al *APIListener) billingProvider() *billing.Service {
	if al.billing == nil {
		return nil
	}
	return al.billing.Provider
}

func (al *APIListener) billingHTTP() *billing.HTTP {
	svc := al.billingProvider()
	if svc == nil {
		return nil
	}
	return billing.NewHTTP(billing.HTTPOptions{
		Service: svc,
		CurrentUser: func(ctx context.Context) (string, error) {
			u, err := al.getUserModelForAuth(ctx)
			if err != nil {
				return "", err
			}
			return u.Username, nil
		},
		Respond: al.responder(),
	})
}

// addBillingProviderRoutes registers the payment provider's routes: the webhook on api (no session
// authentication: the provider's signature is verified), the administrators' payment routes on secureAPI, the
// operators' on operatorAPI (nil without the operator API), and the read-only guard on secureAPI.
func (al *APIListener) addBillingProviderRoutes(api, secureAPI, operatorAPI *mux.Router) {
	h := al.billingHTTP()
	if h == nil {
		return
	}
	api.HandleFunc(billing.WebhookRoute, h.Webhook).Methods(http.MethodPost).Name(billing.WebhookRouteName)
	org := secureAPI.PathPrefix(billing.OrgRoutePrefix).Subrouter()
	org.Use(al.wrapAdminAccessMiddleware)
	h.MountOrg(org)
	if operatorAPI != nil {
		h.MountOperator(operatorAPI.PathPrefix(billing.OperatorRoutePrefix).Subrouter())
	}
}

// billingReadOnly refuses changes of organizations read-only for an overdue payment; next unchanged without a
// payment provider.
func (al *APIListener) billingReadOnly(next http.Handler) http.Handler {
	svc := al.billingProvider()
	if svc == nil {
		return next
	}
	return billing.ReadOnlyGuard(svc, al.responder(), routes.AllRoutesPrefix)(next)
}
