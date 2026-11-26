package chserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/api"
	"github.com/pohcee/procy-server/server/api/users"
	"github.com/pohcee/procy-server/server/chconfig"
	"github.com/pohcee/procy-server/share/tenant"
)

// runWithPermission sends a request as username through permissionsMiddleware(permission),
// using a user store that cannot evaluate group permissions.
func runWithPermission(t *testing.T, username string, permission string) (nextCalled bool, status int) {
	t.Helper()
	bob := &users.User{Username: "bob", Groups: []string{"Operators"}}
	admin := &users.User{Username: "admin", Groups: []string{users.Administrators}}
	svc := users.NewAPIService(users.NewStaticProvider([]*users.User{bob, admin}), false, 0, -1)
	require.False(t, svc.SupportsGroupPermissions(), "test needs a store without group permissions")

	al := &APIListener{
		Logger:      testLog,
		apiServices: &apiServices{config: &chconfig.Config{}},
		userService: &MockUsersService{UserService: svc},
	}
	handler := al.permissionsMiddleware(permission)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalled = true }))

	req := httptest.NewRequest("POST", "/api/v1/commands", nil)
	req = req.WithContext(api.WithUser(tenant.WithID(req.Context(), tenant.Default), username))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return nextCalled, w.Code
}

func TestPermissionsDenyNonAdminsWhenStoreCannotEvaluateThem(t *testing.T) {
	called, status := runWithPermission(t, "bob", users.PermissionCommands)
	assert.False(t, called, "a non-admin must not get commands when permissions can't be evaluated")
	assert.Equal(t, http.StatusForbidden, status)
}

func TestPermissionsAllowAdminsWhenStoreCannotEvaluateThem(t *testing.T) {
	called, _ := runWithPermission(t, "admin", users.PermissionCommands)
	assert.True(t, called)
}

func TestPermissionNoticeHandlesAPIWithoutUserStore(t *testing.T) {
	// With the API disabled there is no user store; the startup notice must not crash rportd.
	assert.NotPanics(t, func() {
		logGroupPermissionsNotice(&users.APIService{}, testLog)
		logGroupPermissionsNotice(nil, testLog)
	})
}
