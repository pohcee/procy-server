package chserver

import (
	"bytes"
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/share/security"
)

// twoFAServiceWithCode returns a service holding a valid code for username.
func twoFAServiceWithCode(username, code string) *TwoFAService {
	srv := NewTwoFAService(600, time.Second, nil, nil)
	srv.tokensByUser[username] = &expirableToken{token: code, expiry: time.Now().Add(time.Minute)}
	return &srv
}

func TestTwoFACodeWorksOnlyOnce(t *testing.T) {
	srv := twoFAServiceWithCode("alice", "123456")

	require.NoError(t, srv.ValidateToken("alice", "123456"))
	assert.Error(t, srv.ValidateToken("alice", "123456"), "a used code must not be accepted again")
}

func TestTwoFACodeIsDiscardedAfterTooManyWrongAttempts(t *testing.T) {
	srv := twoFAServiceWithCode("alice", "123456")

	for i := 0; i < maxTwoFAAttempts; i++ {
		assert.Error(t, srv.ValidateToken("alice", "000000"))
	}
	assert.Error(t, srv.ValidateToken("alice", "123456"), "the code must be discarded after too many wrong guesses")
}

func TestVerify2FAUsesTheLoggedInUserNotTheRequestBody(t *testing.T) {
	// alice started the login (her scoped token authenticated this request); the body names bob.
	al := &APIListener{
		Server: &Server{
			config: &chconfig.Config{API: chconfig.APIConfig{TwoFATokenDelivery: "smtp"}},
		},
		bannedUsers: security.NewBanList(0),
		twoFASrv:    *twoFAServiceWithCode("bob", "654321"),
		Logger:      testLog,
	}
	req := httptest.NewRequest("POST", "/api/v1/verify-2fa", bytes.NewBufferString(`{"username":"bob","token":"654321"}`))
	req = req.WithContext(api.WithUser(context.Background(), "alice"))

	username, err := al.parseAndValidate2FATokenRequest(req)

	assert.Error(t, err, "bob's code must not verify alice's login")
	assert.NotEqual(t, "bob", username)
}
