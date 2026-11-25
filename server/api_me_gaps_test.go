package chserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/chconfig"
)

// Standalone: no email_verified on /me, and no organization, plan catalogue or billing routes.
func TestStandaloneHasNoPostgresOnlyFields(t *testing.T) {
	b, err := json.Marshal(UserPayload{Username: "u"})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "email_verified")
	ev := true
	b, err = json.Marshal(UserPayload{Username: "u", EmailVerified: &ev})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"email_verified":true`)

	al := &APIListener{
		insecureForTests: true,
		apiServices:      &apiServices{config: &chconfig.Config{API: chconfig.APIConfig{MaxRequestBytes: 1024 * 1024}}},
		Logger:           testLog,
	}
	al.initRouter()
	for _, path := range []string{"/api/v1/me/organization", "/api/v1/billing/plans", "/api/v1/operator/billing/plans"} {
		w := httptest.NewRecorder()
		al.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotFound, w.Code, path)
	}
}
