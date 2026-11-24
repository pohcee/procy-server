package chserver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClientsAuthTokenOnlyOnClientsAuthRoutes(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/v1/clients-auth":                         true,
		"/api/v1/clients-auth/":                        true,
		"/api/v1/clients-auth/client-1":                true,
		"/api/v1/clients/x-clients-auth/commands":      false,
		"/api/v1/clients/x-clients-auth":               false,
		"/api/v1/users/clients-auth":                   false,
		"/api/v1/user-groups/clients-auth":             false,
		"/api/v1/clients-authx":                        false,
		"/api/v1/library/entries/clients-auth-cleanup": false,
	} {
		assert.Equal(t, want, isClientsAuthRoute(path), path)
	}
}
