package bearer

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api/session"
	"github.com/openrport/openrport/share/logger"
)

const testSecret = "test-secret-0123456789abcdef"

// memSessions is a minimal in-memory session store with sequential IDs, like the SQLite one.
type memSessions struct {
	next     int64
	sessions map[int64]session.APISession
}

func (m *memSessions) Save(_ context.Context, s session.APISession) (int64, error) {
	if m.sessions == nil {
		m.sessions = map[int64]session.APISession{}
	}
	if s.SessionID == 0 {
		m.next++
		s.SessionID = m.next
	}
	m.sessions[s.SessionID] = s
	return s.SessionID, nil
}

func (m *memSessions) Get(_ context.Context, id int64) (bool, session.APISession, error) {
	s, ok := m.sessions[id]
	return ok, s, nil
}

var testLog = logger.NewLogger("bearer-test", logger.LogOutput{}, logger.LogLevelError)

func validate(t *testing.T, store *memSessions, token string) bool {
	t.Helper()
	tokCtx, err := ParseToken(token, testSecret)
	if err != nil {
		return false
	}
	valid, _, err := ValidateBearerToken(context.Background(), tokCtx, "/api/v1/me", "GET", store, testLog)
	require.NoError(t, err)
	return valid
}

func TestValidTokenIsAccepted(t *testing.T) {
	store := &memSessions{}
	token, err := CreateAuthToken(context.Background(), store, testSecret, time.Hour, "alice", nil, "test", "127.0.0.1")
	require.NoError(t, err)
	assert.True(t, validate(t, store, token))
}

func TestTokenIsRejectedWhenSessionBelongsToAnotherUser(t *testing.T) {
	store := &memSessions{}
	_, err := CreateAuthToken(context.Background(), store, testSecret, time.Hour, "alice", nil, "test", "127.0.0.1")
	require.NoError(t, err)

	// A token naming "admin" but pointing at alice's session (ID 1) must not be accepted.
	forged := jwt.NewWithClaims(jwt.SigningMethodHS256, AppTokenClaims{Username: "admin", SessionID: 1})
	token, err := forged.SignedString([]byte(testSecret))
	require.NoError(t, err)

	assert.False(t, validate(t, store, token))
}

func TestTokenSignedWithAnotherAlgorithmIsRejected(t *testing.T) {
	store := &memSessions{}
	_, err := CreateAuthToken(context.Background(), store, testSecret, time.Hour, "alice", nil, "test", "127.0.0.1")
	require.NoError(t, err)

	other := jwt.NewWithClaims(jwt.SigningMethodHS512, AppTokenClaims{Username: "alice", SessionID: 1})
	token, err := other.SignedString([]byte(testSecret))
	require.NoError(t, err)

	_, err = ParseToken(token, testSecret)
	assert.Error(t, err, "only HS256 tokens are issued, so only HS256 may be accepted")
}
