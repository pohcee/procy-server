package chserver

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/bearer"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/share/security"
	"github.com/openrport/openrport/share/tenant"
)

// twoFAServiceWithCode returns a service holding a valid code for username.
func twoFAServiceWithCode(username, code string) *TwoFAService {
	srv := NewTwoFAService(600, time.Second, nil, nil)
	srv.tokensByUser[twoFAKey{tenant: tenant.Default, username: username}] = &expirableToken{token: code, expiry: time.Now().Add(time.Minute)}
	return &srv
}

func TestTwoFACodeWorksOnlyOnce(t *testing.T) {
	srv := twoFAServiceWithCode("alice", "123456")

	require.NoError(t, srv.ValidateToken(tenantCtx(tenant.Default), "alice", "123456"))
	assert.Error(t, srv.ValidateToken(tenantCtx(tenant.Default), "alice", "123456"), "a used code must not be accepted again")
}

func TestTwoFACodeIsDiscardedAfterTooManyWrongAttempts(t *testing.T) {
	srv := twoFAServiceWithCode("alice", "123456")

	for i := 0; i < maxTwoFAAttempts; i++ {
		assert.Error(t, srv.ValidateToken(tenantCtx(tenant.Default), "alice", "000000"))
	}
	assert.Error(t, srv.ValidateToken(tenantCtx(tenant.Default), "alice", "123456"), "the code must be discarded after too many wrong guesses")
}

func TestVerify2FAUsesTheLoggedInUserNotTheRequestBody(t *testing.T) {
	// alice started the login (her scoped token authenticated this request); the body names bob.
	al := &APIListener{
		apiServices: &apiServices{
			config: &chconfig.Config{API: chconfig.APIConfig{TwoFATokenDelivery: "smtp"}},
		},
		bannedUsers: security.NewBanList(0),
		twoFASrv:    *twoFAServiceWithCode("bob", "654321"),
		Logger:      testLog,
	}
	req := httptest.NewRequest("POST", "/api/v1/verify-2fa", bytes.NewBufferString(`{"username":"bob","token":"654321"}`))
	req = req.WithContext(api.WithUser(tenantCtx(tenant.Default), "alice"))

	username, err := al.parseAndValidate2FATokenRequest(req)

	assert.Error(t, err, "bob's code must not verify alice's login")
	assert.NotEqual(t, "bob", username)
}

// A TotP verification with a token whose login session expired is answered 401 without counting against the address.
func TestVerify2FAWithAStaleSessionIsNotAFailedLogin(t *testing.T) {
	ctx := tenantCtx(tenant.Default)
	al := &APIListener{
		apiServices: &apiServices{config: &chconfig.Config{API: chconfig.APIConfig{TotPEnabled: true, MaxRequestBytes: 1024 * 1024}}},
		apiSessions: newEmptyAPISessionCache(t),
		bannedUsers: security.NewBanList(time.Hour),
		bannedIPs:   security.NewMaxBadAttemptsBanList(2, time.Hour, testLog),
		Logger:      testLog,
	}
	expired, err := bearer.CreateAuthToken(ctx, al.apiSessions, al.config.API.JWTSecret, -time.Hour, "alice", []bearer.Scope{}, "", "")
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("POST", "/api/v1/verify-2fa", bytes.NewBufferString(`{"username":"alice","token":"123456"}`)).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+expired)
		w := httptest.NewRecorder()
		al.handlePostVerify2FAToken().ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	}
	assert.False(t, al.bannedIPs.IsBanned("192.0.2.1"))
}
