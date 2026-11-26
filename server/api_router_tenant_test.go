package chserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pohcee/procy-server/server/chconfig"
	"github.com/pohcee/procy-server/share/tenant"
)

func TestRouterSetsDefaultTenant(t *testing.T) {
	al := &APIListener{
		insecureForTests: true,
		apiServices: &apiServices{
			config: &chconfig.Config{API: chconfig.APIConfig{MaxRequestBytes: 1024 * 1024}},
		},
		Logger: testLog,
	}
	al.initRouter()
	var got tenant.ID
	al.router.HandleFunc("/__tenant_probe", func(_ http.ResponseWriter, r *http.Request) {
		got, _ = tenant.FromContext(r.Context())
	})
	al.router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/__tenant_probe", nil))
	assert.Equal(t, tenant.Default, got)
}
