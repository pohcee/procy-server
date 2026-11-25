package chserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/auditlog"
	auditconfig "github.com/openrport/openrport/server/auditlog/config"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/security"
	"github.com/openrport/openrport/share/tenant"
)

type fakeLoginRecorder struct {
	mu     sync.Mutex
	logins []string
}

func (f *fakeLoginRecorder) RecordLogin(ctx context.Context, username string) error {
	tid, _ := tenant.FromContext(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logins = append(f.logins, string(tid)+"/"+username)
	return nil
}

type memAuditLog struct {
	mu      sync.Mutex
	entries []auditlog.Entry
}

func (m *memAuditLog) Save(_ context.Context, e *auditlog.Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, *e)
	return nil
}
func (m *memAuditLog) List(context.Context, *query.ListOptions) ([]*auditlog.Entry, error) {
	return nil, nil
}
func (m *memAuditLog) Count(context.Context, *query.ListOptions) (int, error) { return 0, nil }
func (m *memAuditLog) Close() error                                           { return nil }

func TestPasswordLoginIsRecordedAndAudited(t *testing.T) {
	user := &users.User{Username: "user1", Password: "$2y$05$ep2DdPDeLDDhwRrED9q/vuVEzRpZtB5WHCFT7YbcmH9r9oNmlsZOm"}
	store := &memAuditLog{}
	al, err := auditlog.New(context.Background(), testLog, noClients{}, auditconfig.Config{Enable: true}, store)
	require.NoError(t, err)
	rec := &fakeLoginRecorder{}
	a := APIListener{
		Logger: testLog,
		apiServices: &apiServices{
			config:   &chconfig.Config{API: chconfig.APIConfig{MaxRequestBytes: 1024 * 1024}},
			auditLog: al,
		},
		loginRecorder: rec,
		bannedUsers:   security.NewBanList(0),
		userService:   users.NewAPIService(users.NewStaticProvider([]*users.User{user}), false, 0, -1),
		apiSessions:   newEmptyAPISessionCache(t),
	}
	a.initRouter()

	login := func(username, password string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/login", nil)
		req.SetBasicAuth(username, password)
		w := httptest.NewRecorder()
		a.router.ServeHTTP(w, req)
		return w.Code
	}
	assert.Equal(t, http.StatusOK, login("user1", "pwd"))
	assert.Equal(t, http.StatusUnauthorized, login("user1", "wrong"))
	assert.Equal(t, http.StatusUnauthorized, login("nobody", "wrong"))

	assert.Equal(t, []string{"default/user1"}, rec.logins, "only the full sign-in is recorded")
	var got []string
	for _, e := range store.entries {
		got = append(got, e.Application+" "+e.Action+" "+e.Username+" "+e.ID)
		assert.Empty(t, e.Request, "no request body (the password) in the audit log")
	}
	assert.Equal(t, []string{"auth.login success user1 user1", "auth.login failed user1 user1"}, got,
		"a login that doesn't exist in the tenant is not audited")
}
