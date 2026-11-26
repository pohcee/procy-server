package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gorilla/mux"

	"github.com/pohcee/procy-server/server/api"
	"github.com/pohcee/procy-server/server/api/users"
	"github.com/pohcee/procy-server/server/billing"
	"github.com/pohcee/procy-server/server/metering"
	"github.com/pohcee/procy-server/server/routes"
	chshare "github.com/pohcee/procy-server/share"
	"github.com/pohcee/procy-server/share/tenant"
)

// RoutePrefix is where the operator API is mounted, below routes.AllRoutesPrefix.
const RoutePrefix = "/operator"

// maxBody bounds the JSON bodies of the operator API.
const maxBody = 64 << 10

// Options are the handler's dependencies; Usage, Billing, Plans, Mounts, OIDCEnabled and Signup are optional.
type Options struct {
	Service Service
	// Usage, when set (usage metering on), serves the usage routes.
	Usage metering.Reader
	// Billing, when set ([billing] enabled), serves the billing routes.
	Billing billing.OperatorService
	// Plans, with Billing, serves the plan catalog.
	Plans *billing.Plans
	// CurrentUser returns the authenticated user of the request (an errors.APIError when there is none).
	CurrentUser func(ctx context.Context) (*users.User, error)
	Respond     *api.Responder
	// Mounts register more operator routes (other packages' operator APIs, such as signup) behind the same check.
	Mounts []func(r *mux.Router)
	// OIDCEnabled: [oidc] sign-in is enabled (an organization may require it only then).
	OIDCEnabled bool
	// Signup: [signup] is enabled (the overview counts the signups awaiting approval).
	Signup bool
}

// Handler serves the operator API.
type Handler struct {
	svc         Service
	usage       metering.Reader
	billing     billing.OperatorService
	plans       *billing.Plans
	currentUser func(ctx context.Context) (*users.User, error)
	respond     *api.Responder
	mounts      []func(r *mux.Router)
	oidcEnabled bool
	signup      bool
}

// NewHandler returns the operator API handler.
func NewHandler(o Options) *Handler {
	return &Handler{svc: o.Service, usage: o.Usage, billing: o.Billing, plans: o.Plans, currentUser: o.CurrentUser, respond: o.Respond, mounts: o.Mounts,
		oidcEnabled: o.OIDCEnabled, signup: o.Signup}
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
	r.HandleFunc("/organizations/{id}/sign-in", h.setSignIn).Methods(http.MethodPut)
	h.mountConsole(r)
	h.mountNodes(r)
	if h.usage != nil {
		r.HandleFunc("/organizations/{id}/usage", h.organizationUsage).Methods(http.MethodGet)
		r.HandleFunc("/usage", h.usageSummary).Methods(http.MethodGet)
	}
	if h.billing != nil {
		h.mountBilling(r)
	}
	for _, m := range h.mounts {
		m(r)
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

// ActorOf is the operator of a request that passed the operator check (a route of Options.Mounts).
func ActorOf(r *http.Request) Actor { return actor(r) }

// actor is the operator of a request that passed requireOperator.
func actor(r *http.Request) Actor {
	name, _ := r.Context().Value(operatorKey{}).(string)
	return Actor{Username: name, RemoteIP: chshare.RemoteIP(r)}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if rng := r.URL.Query().Get("stats"); rng != "" {
		h.listWithStats(w, r, rng)
		return
	}
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
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNodeNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrExists):
		status = http.StatusConflict
	case errors.Is(err, ErrNotActive), errors.Is(err, ErrLastAdmin):
		status = http.StatusConflict
	case errors.Is(err, ErrNotMember), errors.Is(err, ErrNoInvitation):
		status = http.StatusNotFound
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

type signInRequest struct {
	RequireOIDC *bool `json:"require_oidc"`
}

// setSignIn handles PUT /organizations/{id}/sign-in {"require_oidc": bool}.
func (h *Handler) setSignIn(w http.ResponseWriter, r *http.Request) {
	var in signInRequest
	if !h.decode(w, r, &in) {
		return
	}
	if in.RequireOIDC == nil {
		h.fail(w, fmt.Errorf("%w: require_oidc is required", ErrInvalid))
		return
	}
	if *in.RequireOIDC && !h.oidcEnabled {
		h.respond.ErrorWithTitle(w, http.StatusConflict, "OIDC sign-in is not enabled on this server ([oidc])")
		return
	}
	o, err := h.svc.SetRequireOIDC(r.Context(), actor(r), mux.Vars(r)["id"], *in.RequireOIDC)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(o))
}
