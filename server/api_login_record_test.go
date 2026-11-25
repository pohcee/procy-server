package chserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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

// The token of a sign-in that still needs its TOTP code is not a sign-in; the token issued after the code is.
func TestTwoFAPendingLoginIsNotRecorded(t *testing.T) {
	user := &users.User{Username: "user1", Password: "$2y$05$ep2DdPDeLDDhwRrED9q/vuVEzRpZtB5WHCFT7YbcmH9r9oNmlsZOm"}
	store := &memAuditLog{}
	al, err := auditlog.New(context.Background(), testLog, noClients{}, auditconfig.Config{Enable: true}, store)
	require.NoError(t, err)
	rec := &fakeLoginRecorder{}
	a := APIListener{
		Logger: testLog,
		apiServices: &apiServices{
			config:   &chconfig.Config{API: chconfig.APIConfig{MaxRequestBytes: 1024 * 1024, TotPEnabled: true, TotPLoginSessionTimeout: time.Minute}},
			auditLog: al,
		},
		loginRecorder: rec,
		bannedUsers:   security.NewBanList(0),
		userService:   users.NewAPIService(users.NewStaticProvider([]*users.User{user}), false, 0, -1),
		apiSessions:   newEmptyAPISessionCache(t),
		twoFASrv:      NewTwoFAService(600, time.Second, nil, nil),
	}
	a.initRouter()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/login", nil)
	req.SetBasicAuth("user1", "pwd")
	w := httptest.NewRecorder()
	a.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "totp_authenticator_app")
	assert.Empty(t, rec.logins, "the TOTP-pending token is not a sign-in")
	assert.Empty(t, store.entries)

	// after the code (handlePostVerify2FAToken ends in sendJWTToken)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/verify-2fa", nil).WithContext(tenantCtx(tenant.Default))
	w = httptest.NewRecorder()
	a.sendJWTToken("user1", w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []string{"default/user1"}, rec.logins)
	require.Len(t, store.entries, 1)
	assert.Equal(t, "auth.login success", store.entries[0].Application+" "+store.entries[0].Action)
}

// Standalone servers (no login recorder) keep their audit log as it was: no auth.login entries.
func TestStandaloneLoginIsNotAudited(t *testing.T) {
	user := &users.User{Username: "user1", Password: "$2y$05$ep2DdPDeLDDhwRrED9q/vuVEzRpZtB5WHCFT7YbcmH9r9oNmlsZOm"}
	store := &memAuditLog{}
	al, err := auditlog.New(context.Background(), testLog, noClients{}, auditconfig.Config{Enable: true}, store)
	require.NoError(t, err)
	a := APIListener{
		Logger:      testLog,
		apiServices: &apiServices{config: &chconfig.Config{API: chconfig.APIConfig{MaxRequestBytes: 1024 * 1024}}, auditLog: al},
		bannedUsers: security.NewBanList(0),
		userService: users.NewAPIService(users.NewStaticProvider([]*users.User{user}), false, 0, -1),
		apiSessions: newEmptyAPISessionCache(t),
	}
	a.initRouter()
	for _, pwd := range []string{"pwd", "wrong"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/login", nil)
		req.SetBasicAuth("user1", pwd)
		a.router.ServeHTTP(httptest.NewRecorder(), req)
	}
	assert.Empty(t, store.entries)
}
