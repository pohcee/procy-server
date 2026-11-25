package chserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/openrport/openrport/server/chconfig"
)

// A standalone server (no [signup]) has none of the signup routes and reports no signup capability.
func TestNoSignupAPIStandalone(t *testing.T) {
	al := &APIListener{
		insecureForTests: true,
		apiServices:      &apiServices{config: &chconfig.Config{API: chconfig.APIConfig{MaxRequestBytes: 1024 * 1024}}},
		Logger:           testLog,
	}
	al.initRouter()
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/signup"},
		{http.MethodPost, "/api/v1/signup"},
		{http.MethodPost, "/api/v1/signup/verify"},
		{http.MethodPost, "/api/v1/signup/cancel-deletion"},
		{http.MethodGet, "/api/v1/organization/export"},
		{http.MethodPost, "/api/v1/organization/deletion"},
		{http.MethodGet, "/api/v1/operator/signups"},
	} {
		w := httptest.NewRecorder()
		al.router.ServeHTTP(w, httptest.NewRequest(rt.method, rt.path, strings.NewReader("{}")))
		assert.Equal(t, http.StatusNotFound, w.Code, rt.method+" "+rt.path)
	}
	data := getStatusData(t, newStatusTestListener(&chconfig.Config{}, nil))
	assert.NotContains(t, string(data["capabilities"]), "signup")
}
