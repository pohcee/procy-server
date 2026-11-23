package chserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api/authorization"
	"github.com/openrport/openrport/server/api/session"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/routes"
)

// deletableUsersService accepts Delete, which the single-user provider doesn't support.
type deletableUsersService struct {
	UserService
}

func (deletableUsersService) Delete(string) error { return nil }

func TestDeletingAUserRevokesTheirSessionsAndAPITokens(t *testing.T) {
	ctx := context.Background()
	sessions := newEmptyAPISessionCache(t)
	_, err := sessions.Save(ctx, session.APISession{Username: "bob", ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)

	tokens := authorization.NewManager(
		CommonAPITokenTestDb(t, "bob", "bobprefx", "bob's token", authorization.APITokenReadWrite, "bobs-secret-token-value-0123456789"))

	al := &APIListener{
		Logger:       testLog,
		Server:       &Server{config: &chconfig.Config{}},
		userService:  deletableUsersService{UserService: users.NewAPIService(users.NewStaticProvider(nil), false, 0, -1)},
		apiSessions:  sessions,
		tokenManager: tokens,
	}

	req := httptest.NewRequest("DELETE", "/api/v1/users/bob", nil)
	req = mux.SetURLVars(req, map[string]string{routes.ParamUserID: "bob"})
	w := httptest.NewRecorder()
	al.handleDeleteUser(w, req)
	require.Equal(t, http.StatusNoContent, w.Code)

	remaining, err := sessions.GetAllByUser(ctx, "bob")
	require.NoError(t, err)
	assert.Empty(t, remaining, "a deleted user's sessions must be revoked")

	left, err := tokens.GetAll(ctx, "bob")
	require.NoError(t, err)
	assert.Empty(t, left, "a deleted user's API tokens must be revoked")
}
