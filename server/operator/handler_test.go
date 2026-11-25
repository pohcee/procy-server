package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/routes"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

type fakeService struct {
	calls []string
	limit *int
	by    Actor
	err   error
}

func (f *fakeService) List(context.Context) ([]Organization, error) {
	f.calls = append(f.calls, "list")
	return nil, f.err
}

func (f *fakeService) Get(_ context.Context, id string) (*Organization, error) {
	f.calls = append(f.calls, "get "+id)
	return &Organization{ID: id}, f.err
}

func (f *fakeService) Create(_ context.Context, by Actor, o NewOrganization) (*Organization, error) {
	f.calls = append(f.calls, "create "+o.ID)
	f.by, f.limit = by, o.DeviceLimit
	return &Organization{ID: o.ID, DeviceLimit: o.DeviceLimit}, f.err
}

func (f *fakeService) SetStatus(_ context.Context, by Actor, id, status string) (*Organization, error) {
	f.calls = append(f.calls, status+" "+id)
	f.by = by
	return &Organization{ID: id, Status: status}, f.err
}

func (f *fakeService) SetDeviceLimit(_ context.Context, by Actor, id string, limit *int) (*Organization, error) {
	f.calls = append(f.calls, "limit "+id)
	f.by, f.limit = by, limit
	return &Organization{ID: id, DeviceLimit: limit}, f.err
}

func (f *fakeService) AddAdmin(_ context.Context, by Actor, id string, a NewAdmin) (*Admin, error) {
	f.calls = append(f.calls, "admin "+id+" "+a.Username)
	f.by = by
	return &Admin{Organization: id, Username: a.Username, NewAccount: true, Password: "pw", PasswordExpired: true}, f.err
}

func (f *fakeService) Members(_ context.Context, id string) ([]Member, error) {
	f.calls = append(f.calls, "members "+id)
	return nil, f.err
}

type testUser struct {
	tid  tenant.ID
	user *users.User
}

// newRouter is the operator API behind a fake authentication that puts the tenant and user in the context.
func newRouter(svc Service, who testUser) *mux.Router {
	log := logger.NewLogger("test", logger.LogOutput{File: nil}, logger.LogLevelError)
	h := NewHandler(Options{
		Service: svc,
		CurrentUser: func(context.Context) (*users.User, error) {
			return who.user, nil
		},
		Respond: &api.Responder{Log: log},
	})
	r := mux.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(tenant.WithID(req.Context(), who.tid)))
		})
	})
	secure := r.PathPrefix(routes.AllRoutesPrefix).Subrouter()
	secure.Use(SystemTenantGuard(&api.Responder{Log: log}))
	h.Mount(secure.PathPrefix(RoutePrefix).Subrouter())
	secure.HandleFunc("/clients", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	secure.HandleFunc("/me", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	secure.HandleFunc("/me/tokens", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	secure.HandleFunc("/users/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	secure.HandleFunc("/usersx", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	return r
}

var (
	operatorUser = testUser{tid: SystemTenant, user: &users.User{Username: "op", Groups: []string{users.Administrators}}}
	systemViewer = testUser{tid: SystemTenant, user: &users.User{Username: "viewer", Groups: []string{"Viewers"}}}
	orgAdmin     = testUser{tid: "acme", user: &users.User{Username: "alice", Groups: []string{users.Administrators}}}
)

func do(t *testing.T, r http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, routes.AllRoutesPrefix+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	out["_raw"] = w.Body.String()
	return w.Code, out
}

func TestOperatorRoutesOnlyForSystemAdmins(t *testing.T) {
	routesToTry := []struct{ method, path, body string }{
		{http.MethodGet, "/operator/organizations", ""},
		{http.MethodPost, "/operator/organizations", `{"id":"x"}`},
		{http.MethodGet, "/operator/organizations/x", ""},
		{http.MethodPost, "/operator/organizations/x/suspend", ""},
		{http.MethodPost, "/operator/organizations/x/activate", ""},
		{http.MethodPut, "/operator/organizations/x/device-limit", `{"device_limit":1}`},
		{http.MethodPost, "/operator/organizations/x/admins", `{"username":"u"}`},
		{http.MethodGet, "/operator/organizations/x/members", ""},
	}
	for _, tc := range []struct {
		who  testUser
		want int
	}{{orgAdmin, http.StatusNotFound}, {testUser{tid: tenant.Default, user: orgAdmin.user}, http.StatusNotFound}, {systemViewer, http.StatusForbidden}} {
		svc := &fakeService{}
		r := newRouter(svc, tc.who)
		for _, rt := range routesToTry {
			code, out := do(t, r, rt.method, rt.path, rt.body)
			assert.Equal(t, tc.want, code, "%s %s as %s/%s: %s", rt.method, rt.path, tc.who.tid, tc.who.user.Username, out["_raw"])
		}
		assert.Empty(t, svc.calls)
	}
}

func TestOperatorRoutes(t *testing.T) {
	svc := &fakeService{}
	r := newRouter(svc, operatorUser)

	code, out := do(t, r, http.MethodGet, "/operator/organizations", "")
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, []any{}, out["data"], "an empty list, not null")

	code, out = do(t, r, http.MethodPost, "/operator/organizations", `{"id":"acme","name":"Acme","device_limit":5}`)
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	assert.Equal(t, 5, *svc.limit)
	assert.Equal(t, "op", svc.by.Username)

	code, out = do(t, r, http.MethodPost, "/operator/organizations", `{"id":"acme","bogus":1}`)
	assert.Equal(t, http.StatusBadRequest, code, out["_raw"])

	code, out = do(t, r, http.MethodPost, "/operator/organizations/acme/suspend", "")
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, StatusSuspended, out["data"].(map[string]any)["status"])
	code, _ = do(t, r, http.MethodPost, "/operator/organizations/acme/activate", "")
	require.Equal(t, http.StatusOK, code)

	code, out = do(t, r, http.MethodPut, "/operator/organizations/acme/device-limit", `{"device_limit":null}`)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Nil(t, svc.limit, "null is unlimited")
	code, out = do(t, r, http.MethodPut, "/operator/organizations/acme/device-limit", `{"device_limit":7}`)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, 7, *svc.limit)
	for _, bad := range []string{`{}`, `{"device_limit":"7"}`, `{"device_limit":1.5}`, ``} {
		code, out = do(t, r, http.MethodPut, "/operator/organizations/acme/device-limit", bad)
		assert.Equal(t, http.StatusBadRequest, code, "%s: %s", bad, out["_raw"])
	}

	code, out = do(t, r, http.MethodPost, "/operator/organizations/acme/admins", `{"username":"carol"}`)
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	assert.Equal(t, map[string]any{"organization": "acme", "username": "carol", "new_account": true, "password": "pw", "password_expired": true}, out["data"])

	code, out = do(t, r, http.MethodGet, "/operator/organizations/acme/members", "")
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, []any{}, out["data"], "an empty list, not null")

	assert.Equal(t, []string{"list", "create acme", "suspended acme", "active acme", "limit acme", "limit acme", "admin acme carol", "members acme"}, svc.calls)
}

func TestOperatorErrors(t *testing.T) {
	for err, want := range map[error]int{
		ErrNotFound:                       http.StatusNotFound,
		fmt.Errorf("x %w", ErrExists):     http.StatusConflict,
		ErrNotActive:                      http.StatusConflict,
		fmt.Errorf("%w: bad", ErrInvalid): http.StatusBadRequest,
		fmt.Errorf("boom"):                http.StatusInternalServerError,
	} {
		r := newRouter(&fakeService{err: err}, operatorUser)
		code, out := do(t, r, http.MethodGet, "/operator/organizations/acme", "")
		assert.Equal(t, want, code, "%v: %s", err, out["_raw"])
	}
}

func TestSystemTenantGuard(t *testing.T) {
	r := newRouter(&fakeService{}, operatorUser)
	for path, want := range map[string]int{
		"/clients":   http.StatusForbidden,
		"/usersx":    http.StatusForbidden,
		"/me":        http.StatusTeapot,
		"/me/tokens": http.StatusTeapot,
		"/users/bob": http.StatusTeapot,
	} {
		code, out := do(t, r, http.MethodGet, path, "")
		assert.Equal(t, want, code, "%s: %s", path, out["_raw"])
	}
	r = newRouter(&fakeService{}, orgAdmin)
	code, _ := do(t, r, http.MethodGet, "/clients", "")
	assert.Equal(t, http.StatusTeapot, code, "other tenants are not restricted")
}

func TestIsOperator(t *testing.T) {
	for who, want := range map[*testUser]bool{&operatorUser: true, &systemViewer: false, &orgAdmin: false} {
		h := NewHandler(Options{Service: &fakeService{}, CurrentUser: func(context.Context) (*users.User, error) { return who.user, nil }})
		assert.Equal(t, want, h.IsOperator(tenant.WithID(context.Background(), who.tid)), who.user.Username)
	}
	h := NewHandler(Options{Service: &fakeService{}, CurrentUser: func(context.Context) (*users.User, error) { return nil, nil }})
	assert.False(t, h.IsOperator(tenant.WithID(context.Background(), SystemTenant)), "no user")
	assert.False(t, h.IsOperator(context.Background()), "no tenant")
}
