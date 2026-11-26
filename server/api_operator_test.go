package chserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pohcee/procy-server/server/chconfig"
)

// A server without an operator service (standalone) has no operator routes and reports no operator capability.
func TestNoOperatorAPIWithoutOperatorService(t *testing.T) {
	al := &APIListener{
		insecureForTests: true,
		apiServices: &apiServices{
			config: &chconfig.Config{API: chconfig.APIConfig{MaxRequestBytes: 1024 * 1024}},
		},
		Logger: testLog,
	}
	al.initRouter()
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/operator/organizations"},
		{http.MethodPost, "/api/v1/operator/organizations"},
		{http.MethodGet, "/api/v1/operator/organizations/acme"},
		{http.MethodPost, "/api/v1/operator/organizations/acme/suspend"},
		{http.MethodPut, "/api/v1/operator/organizations/acme/device-limit"},
		{http.MethodPost, "/api/v1/operator/organizations/acme/admins"},
		{http.MethodGet, "/api/v1/operator/nodes"},
		{http.MethodGet, "/api/v1/operator/nodes/n1"},
		{http.MethodGet, "/api/v1/operator/nodes/n1/devices"},
	} {
		w := httptest.NewRecorder()
		al.router.ServeHTTP(w, httptest.NewRequest(rt.method, rt.path, nil))
		assert.Equal(t, http.StatusNotFound, w.Code, "%s %s", rt.method, rt.path)
	}

	data := getStatusData(t, newStatusTestListener(&chconfig.Config{}, nil))
	assert.NotContains(t, string(data["capabilities"]), "operator")
}
