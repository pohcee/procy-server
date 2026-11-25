package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gorilla/mux"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/billing"
	"github.com/openrport/openrport/server/metering"
	"github.com/openrport/openrport/server/routes"
	chshare "github.com/openrport/openrport/share"
	"github.com/openrport/openrport/share/tenant"
)

// RoutePrefix is where the operator API is mounted, below routes.AllRoutesPrefix.
const RoutePrefix = "/operator"

// maxBody bounds the JSON bodies of the operator API.
const maxBody = 64 << 10

// Options are the handler's dependencies; every field is required except Usage.
type Options struct {
	Service Service
	// Usage, when set (usage metering on), serves the usage routes.
	Usage metering.Reader
	// Billing, when set ([billing] enabled), serves the billing routes.
	Billing billing.OperatorService
	// CurrentUser returns the authenticated user of the request (an errors.APIError when there is none).
	CurrentUser func(ctx context.Context) (*users.User, error)
	Respond     *api.Responder
}

// Handler serves the operator API.
type Handler struct {
	svc         Service
	usage       metering.Reader
	billing     billing.OperatorService
	currentUser func(ctx context.Context) (*users.User, error)
	respond     *api.Responder
}

// NewHandler returns the operator API handler.
func NewHandler(o Options) *Handler {
	return &Handler{svc: o.Service, usage: o.Usage, billing: o.Billing, currentUser: o.CurrentUser, respond: o.Respond}
}

// Mount registers the operator routes on r, a router for RoutePrefix behind authentication.
func (h *Handler) Mount(r *mux.Router) {
	r.Use(h.requireOperator)
	r.HandleFunc("/organizations", h.list).Methods(http.MethodGet)
	r.HandleFunc("/organizations", h.create).Methods(http.MethodPost)
	r.HandleFunc("/organizations/{id}", h.get).Methods(http.MethodGet)
	r.HandleFunc("/organizations/{id}/suspend", h.setStatus(StatusSuspended)).Methods(http.MethodPost)
	r.HandleFunc("/organizations/{id}/activate", h.setStatus(StatusActive)).Methods(http.MethodPost)
	r.HandleFunc("/organizations/{id}/device-limit", h.setDeviceLimit).Methods(http.MethodPut)
	r.HandleFunc("/organizations/{id}/admins", h.addAdmin).Methods(http.MethodPost)
	r.HandleFunc("/organizations/{id}/members", h.members).Methods(http.MethodGet)
	if h.usage != nil {
		r.HandleFunc("/organizations/{id}/usage", h.organizationUsage).Methods(http.MethodGet)
		r.HandleFunc("/usage", h.usageSummary).Methods(http.MethodGet)
	}
	if h.billing != nil {
		h.mountBilling(r)
	}
}

// IsOperator reports whether the request comes from a platform operator: an administrator of SystemTenant.
func (h *Handler) IsOperator(ctx context.Context) bool {
	if id, err := tenant.FromContext(ctx); err != nil || id != SystemTenant {
		return false
	}
	u, err := h.currentUser(ctx)
	return err == nil && u != nil && u.IsAdmin()
}

// requireOperator answers 404 outside the system tenant (the operator API doesn't exist for organizations) and 403
// to users of the system tenant who aren't administrators.
func (h *Handler) requireOperator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, err := tenant.FromContext(r.Context()); err != nil || id != SystemTenant {
			h.respond.ErrorWithTitle(w, http.StatusNotFound, "not found")
			return
		}
		u, err := h.currentUser(r.Context())
		if err != nil {
			h.respond.Error(w, err)
			return
		}
		if !u.IsAdmin() {
			h.respond.ErrorWithTitle(w, http.StatusForbidden, "only platform operators (administrators of the system organization) may use the operator API")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), operatorKey{}, u.Username)))
	})
}

type operatorKey struct{}

// actor is the operator of a request that passed requireOperator.
func actor(r *http.Request) Actor {
	name, _ := r.Context().Value(operatorKey{}).(string)
	return Actor{Username: name, RemoteIP: chshare.RemoteIP(r)}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	orgs, err := h.svc.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	if orgs == nil {
		orgs = []Organization{}
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(orgs))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	o, err := h.svc.Get(r.Context(), mux.Vars(r)["id"])
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(o))
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var in NewOrganization
	if !h.decode(w, r, &in) {
		return
	}
	o, err := h.svc.Create(r.Context(), actor(r), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusCreated, api.NewSuccessPayload(o))
}

func (h *Handler) setStatus(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		o, err := h.svc.SetStatus(r.Context(), actor(r), mux.Vars(r)["id"], status)
		if err != nil {
			h.fail(w, err)
			return
		}
		h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(o))
	}
}

// deviceLimitRequest needs the field present: {"device_limit": null} is unlimited, a missing field is an error.
type deviceLimitRequest struct {
	DeviceLimit json.RawMessage `json:"device_limit"`
}

func (h *Handler) setDeviceLimit(w http.ResponseWriter, r *http.Request) {
	var in deviceLimitRequest
	if !h.decode(w, r, &in) {
		return
	}
	if len(in.DeviceLimit) == 0 {
		h.fail(w, fmt.Errorf("%w: device_limit is required (null for unlimited)", ErrInvalid))
		return
	}
	var limit *int
	if err := json.Unmarshal(in.DeviceLimit, &limit); err != nil {
		h.fail(w, fmt.Errorf("%w: device_limit must be a whole number or null", ErrInvalid))
		return
	}
	o, err := h.svc.SetDeviceLimit(r.Context(), actor(r), mux.Vars(r)["id"], limit)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(o))
}

func (h *Handler) addAdmin(w http.ResponseWriter, r *http.Request) {
	var in NewAdmin
	if !h.decode(w, r, &in) {
		return
	}
	a, err := h.svc.AddAdmin(r.Context(), actor(r), mux.Vars(r)["id"], in)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusCreated, api.NewSuccessPayload(a))
}

func (h *Handler) members(w http.ResponseWriter, r *http.Request) {
	ms, err := h.svc.Members(r.Context(), mux.Vars(r)["id"])
	if err != nil {
		h.fail(w, err)
		return
	}
	if ms == nil {
		ms = []Member{}
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(ms))
}

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		h.fail(w, fmt.Errorf("%w: %v", ErrInvalid, err))
		return false
	}
	return true
}

// fail answers err with the status of its kind; errors.APIError(s) from the user service keep their own.
func (h *Handler) fail(w http.ResponseWriter, err error) {
	status := 0
	switch {
	case errors.Is(err, metering.ErrInvalid), errors.Is(err, billing.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, billing.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrExists):
		status = http.StatusConflict
	case errors.Is(err, ErrNotActive):
		status = http.StatusConflict
	case errors.Is(err, ErrInvalid):
		status = http.StatusBadRequest
	}
	if status != 0 {
		h.respond.ErrorWithTitle(w, status, err.Error())
		return
	}
	h.respond.Error(w, err)
}

// systemTenantRoutes are the routes (below routes.AllRoutesPrefix) the system tenant is served: its own account,
// tokens and 2FA, its users and groups (the operators), its audit log and the operator API.
var systemTenantRoutes = []string{
	"/status", "/me", "/users", "/user-groups", "/auditlog", RoutePrefix,
}

// SystemTenantGuard serves requests of the system tenant only on the routes it needs (systemTenantRoutes) and
// answers 403 on every other route, so platform operators act on organizations only through the operator API.
// Requests of other tenants pass unchanged. Put it after authentication.
func SystemTenantGuard(respond *api.Responder) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if id, err := tenant.FromContext(r.Context()); err != nil || id != SystemTenant || systemTenantRoute(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			respond.ErrorWithTitle(w, http.StatusForbidden, "not available in the system organization; use the operator API")
		})
	}
}

func systemTenantRoute(path string) bool {
	rest, ok := strings.CutPrefix(path, routes.AllRoutesPrefix)
	if !ok {
		return false
	}
	for _, p := range systemTenantRoutes {
		if rest == p || strings.HasPrefix(rest, p+"/") {
			return true
		}
	}
	return false
}
