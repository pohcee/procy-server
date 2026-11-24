package chserver

import (
	"context"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api/message"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/share/security"
	"github.com/openrport/openrport/share/tenant"
)

func tenantCtx(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

// capturingMsgSrv remembers the last 2FA code it was asked to send.
type capturingMsgSrv struct {
	message.ServiceMock
	lastToken string
}

func (s *capturingMsgSrv) Send(_ context.Context, data message.Data) error {
	s.lastToken = data.Token
	return nil
}

func newTenantTwoFAService() (*TwoFAService, *capturingMsgSrv) {
	msg := &capturingMsgSrv{}
	srv := NewTwoFAService(600, time.Second, usrService{twoFASendTo: "alice@example.com"}, msg)
	return &srv, msg
}

func TestTwoFARequiresTenant(t *testing.T) {
	srv, _ := newTenantTwoFAService()
	ctx := context.Background()

	_, err := srv.SendToken(ctx, "alice", "ua", "1.2.3.4")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.ErrorIs(t, srv.ValidateToken(ctx, "alice", "123456"), tenant.ErrMissing)
	assert.ErrorIs(t, srv.SetTotPLoginSession(ctx, "alice", time.Minute), tenant.ErrMissing)
	assert.ErrorIs(t, srv.ValidateTotPCode(ctx, &users.User{Username: "alice"}, "123456"), tenant.ErrMissing)
}

func TestTwoFACodeIsScopedToTenant(t *testing.T) {
	srv, msg := newTenantTwoFAService()
	a, b := tenantCtx("a"), tenantCtx("b")

	_, err := srv.SendToken(a, "alice", "ua", "1.2.3.4")
	require.NoError(t, err)
	codeA := msg.lastToken
	// alice of tenant b logs in too; that must not replace the code of alice in tenant a
	_, err = srv.SendToken(b, "alice", "ua", "1.2.3.4")
	require.NoError(t, err)
	codeB := msg.lastToken

	if codeA != codeB {
		assert.Error(t, srv.ValidateToken(b, "alice", codeA), "a's code must not verify b's login")
	}
	require.NoError(t, srv.ValidateToken(a, "alice", codeA))
	require.NoError(t, srv.ValidateToken(b, "alice", codeB))
}

func TestTwoFAWrongGuessesInOneTenantDontDiscardAnother(t *testing.T) {
	srv, msg := newTenantTwoFAService()
	a, b := tenantCtx("a"), tenantCtx("b")

	_, err := srv.SendToken(a, "alice", "ua", "1.2.3.4")
	require.NoError(t, err)
	codeA := msg.lastToken
	for i := 0; i < maxTwoFAAttempts; i++ {
		assert.Error(t, srv.ValidateToken(b, "alice", "000000"))
	}
	require.NoError(t, srv.ValidateToken(a, "alice", codeA))
}

func TestTotPLoginSessionIsScopedToTenant(t *testing.T) {
	srv, _ := newTenantTwoFAService()
	a, b := tenantCtx("a"), tenantCtx("b")

	usr := &users.User{Username: "alice"}
	key, err := GenerateTotPSecretKey(&TotPInput{Issuer: "iss", AccountName: "alice"})
	require.NoError(t, err)
	StoreTotPCodeInUser(usr, key)

	require.NoError(t, srv.SetTotPLoginSession(a, "alice", time.Minute))
	code, err := totp.GenerateCode(key.Secret, time.Now())
	require.NoError(t, err)

	assert.EqualError(t, srv.ValidateTotPCode(b, usr, code), "login request not found for provided username")
	require.NoError(t, srv.ValidateTotPCode(a, usr, code))
}

func TestBannedUserIsScopedToTenant(t *testing.T) {
	al := &APIListener{bannedUsers: security.NewBanList(time.Minute)}
	a, b := tenantCtx("a"), tenantCtx("b")

	al.banUser(a, "alice")
	assert.True(t, al.isUserBanned(a, "alice"))
	assert.False(t, al.isUserBanned(b, "alice"))
	assert.False(t, al.isUserBanned(a, "bob"))
	// a request without a tenant never shares the ban state of a real tenant
	assert.False(t, al.isUserBanned(context.Background(), "alice"))
}

func TestBasicAuthLooksUpUsersInTheRequestTenant(t *testing.T) {
	alice := &users.User{Username: "alice", Password: "secret"}
	al := &APIListener{
		apiServices: &apiServices{config: &chconfig.Config{}},
		Logger:      testLog,
		bannedUsers: security.NewBanList(0),
		userService: users.NewAPIService(users.NewStaticProvider([]*users.User{alice}), false, 0, -1),
	}

	ok, _, err := al.handleBasicAuth(tenantCtx(tenant.Default), "GET", "/api/v1/me", "alice", "secret")
	require.NoError(t, err)
	assert.True(t, ok)

	ok, _, err = al.handleBasicAuth(tenantCtx("other"), "GET", "/api/v1/me", "alice", "secret")
	assert.ErrorIs(t, err, tenant.ErrNotSupported)
	assert.False(t, ok)

	ok, _, err = al.handleBasicAuth(context.Background(), "GET", "/api/v1/me", "alice", "secret")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.False(t, ok)
}
