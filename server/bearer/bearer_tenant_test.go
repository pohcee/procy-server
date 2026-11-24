package bearer

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/share/tenant"
)

func tenantCtx(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

func validateIn(ctx context.Context, t *testing.T, store *memSessions, token string) bool {
	t.Helper()
	tokCtx, err := ParseToken(token, testSecret)
	require.NoError(t, err)
	valid, _, err := ValidateBearerToken(ctx, tokCtx, "/api/v1/me", "GET", store, testLog)
	require.NoError(t, err)
	return valid
}

func TestCreateAuthTokenCarriesTenant(t *testing.T) {
	store := &memSessions{}
	token, err := CreateAuthToken(tenantCtx("a"), store, testSecret, time.Hour, "alice", nil, "test", "127.0.0.1")
	require.NoError(t, err)

	tokCtx, err := ParseToken(token, testSecret)
	require.NoError(t, err)
	assert.Equal(t, tenant.ID("a"), tokCtx.AppClaims.TenantID)
	assert.True(t, validateIn(tenantCtx("a"), t, store, token))
}

func TestCreateAuthTokenRequiresTenant(t *testing.T) {
	store := &memSessions{}
	_, err := CreateAuthToken(context.Background(), store, testSecret, time.Hour, "alice", nil, "test", "127.0.0.1")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.Empty(t, store.sessions, "no session is saved")
}

func TestBearerTokenOfOtherTenantRejected(t *testing.T) {
	// one store for both tenants, so only the tenant claim can tell the token apart
	store := &memSessions{}
	token, err := CreateAuthToken(tenantCtx("a"), store, testSecret, time.Hour, "alice", nil, "test", "127.0.0.1")
	require.NoError(t, err)

	assert.False(t, validateIn(tenantCtx("b"), t, store, token))
	assert.False(t, validateIn(tenantCtx(tenant.Default), t, store, token))
}

func TestBearerTokenWithoutTenantOnlyDefault(t *testing.T) {
	store := &memSessions{}
	_, err := CreateAuthToken(defaultCtx(), store, testSecret, time.Hour, "alice", nil, "test", "127.0.0.1")
	require.NoError(t, err)

	// a token issued before tokens carried the tenant
	old := jwt.NewWithClaims(jwt.SigningMethodHS256, AppTokenClaims{Username: "alice", SessionID: 1})
	token, err := old.SignedString([]byte(testSecret))
	require.NoError(t, err)

	assert.True(t, validateIn(defaultCtx(), t, store, token))
	assert.False(t, validateIn(tenantCtx("a"), t, store, token))
}
