package chserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/cgroups"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/server/routes"
)

type failingClientGroupProvider struct {
	cgroups.ClientGroupProvider
}

func (failingClientGroupProvider) GetAll(context.Context) ([]*cgroups.ClientGroup, error) {
	return nil, errors.New("client groups unavailable")
}

func TestClientAccessMiddlewareStopsWhenClientGroupsCannotBeLoaded(t *testing.T) {
	keepDisconnected := time.Hour
	c1 := clients.New(t).ID("client-1").ClientAuthID(cl1.ID).Logger(testLog).Build()
	admin := &users.User{Username: "admin", Groups: []string{users.Administrators}}
	al := &APIListener{
		Logger: testLog,
		apiServices: &apiServices{
			config:              &chconfig.Config{},
			clientService:       clients.NewClientService(nil, nil, clients.NewClientRepository([]*clientdata.Client{c1}, &keepDisconnected, testLog), testLog, nil),
			clientGroupProvider: failingClientGroupProvider{},
		},
		userService: &MockUsersService{UserService: users.NewAPIService(users.NewStaticProvider([]*users.User{admin}), false, 0, -1)},
	}
	useLocalDispatcher(al)

	nextCalled := false
	handler := al.wrapClientAccessMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalled = true }))

	req := httptest.NewRequest("GET", "/api/v1/clients/client-1", nil)
	req = mux.SetURLVars(req, map[string]string{routes.ParamClientID: "client-1"})
	req = req.WithContext(api.WithUser(req.Context(), "admin"))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.False(t, nextCalled, "the handler must not run when the access check could not be completed")
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
