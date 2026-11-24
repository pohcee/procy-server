package chserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClientAttributesHandlersWithoutClientInContext(t *testing.T) {
	al := &APIListener{Logger: testLog}

	for name, h := range map[string]http.HandlerFunc{
		"get":    al.handleGetClientAttributes,
		"update": al.handleUpdateClientAttributes,
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/api/v1/clients/x/attributes", strings.NewReader(`{"tags":["a"]}`))

			assert.NotPanics(t, func() { h(rec, req) })
			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.Contains(t, rec.Body.String(), "client not present in the request")
		})
	}
}
