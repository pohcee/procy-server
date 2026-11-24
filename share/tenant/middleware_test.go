package tenant_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/openrport/openrport/share/tenant"
)

type failingResolver struct{}

func (failingResolver) Resolve(*http.Request) (tenant.ID, error) { return "", errors.New("nope") }

func TestMiddlewareSetsResolvedTenant(t *testing.T) {
	var got tenant.ID
	h := tenant.Middleware(tenant.DefaultResolver{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = tenant.FromContext(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, tenant.Default, got)
}

func TestMiddlewareRejectsUnknownTenant(t *testing.T) {
	called := false
	h := tenant.Middleware(failingResolver{})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.False(t, called)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "unknown tenant\n", rec.Body.String())
}
