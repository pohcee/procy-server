package chserver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsClientsAuthRoute(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/v1/clients-auth":      true,
		"/api/v1/clients-auth/dev1": true,
		"/api/v1/clients-authx":     false,
		"/api/v1/clients":           false,
		"/api/v1/operator/organizations/acme-clients-auth/admins": false,
		"/api/v1/me?x=clients-auth":                               false,
	} {
		assert.Equal(t, want, isClientsAuthRoute(path), path)
	}
}
