package chserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/pairing"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/security"
)

var pairingDepositBody = map[string]string{
	"connect_url": "https://rport.example.com:8080",
	"fingerprint": "2a:c4:79:04:80:ba:7c:60:05:e5:2c:49:6d:74:56:24",
	"client_id":   "client1",
	"password":    "foobaz",
}

func newPairingTestListener(t *testing.T, insecure bool, docRoot string) *APIListener {
	t.Helper()
	userService := users.NewAPIService(users.NewStaticProvider([]*users.User{
		{Username: "admin", Password: "adminpass", Groups: []string{users.Administrators}},
		{Username: "operator", Password: "operatorpass", Groups: []string{"operators"}},
	}), false, 0, -1)
	al := &APIListener{
		insecureForTests: insecure,
		Logger:           logger.NewLogger("api-listener-test", logger.LogOutput{File: os.Stdout}, logger.LogLevelError),
		Server: &Server{
			config: &chconfig.Config{
				API: chconfig.APIConfig{
					DocRoot:         docRoot,
					MaxRequestBytes: 10 * 1024,
				},
				Server: chconfig.ServerConfig{PairingURL: "https://pairing.example.com"},
				Pairing: chconfig.PairingConfig{
					Enabled:      true,
					URL:          "https://rport.example.com",
					DownloadsURL: "https://rport.example.com/downloads",
					CodeTTL:      time.Hour,
				},
			},
		},
		bannedUsers: security.NewBanList(0),
		userService: &MockUsersService{UserService: userService},
	}
	al.initRouter()
	return al
}

func postDeposit(al *APIListener, user, password string) *httptest.ResponseRecorder {
	payload, _ := json.Marshal(pairingDepositBody)
	req := httptest.NewRequest(http.MethodPost, "/pairing", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	rec := httptest.NewRecorder()
	al.router.ServeHTTP(rec, req)
	return rec
}

func TestPairingDepositRequiresAuthenticatedAdmin(t *testing.T) {
	al := newPairingTestListener(t, false, "")

	rec := postDeposit(al, "", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "no credentials")

	rec = postDeposit(al, "admin", "wrong")
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "wrong password")

	rec = postDeposit(al, "operator", "operatorpass")
	assert.Equal(t, http.StatusForbidden, rec.Code, "not an administrator")

	rec = postDeposit(al, "admin", "adminpass")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp pairing.Response
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp.PairingCode, pairing.CodeLength)
	assert.Contains(t, resp.Installers.Linux, "curl https://rport.example.com/pairing/"+resp.PairingCode+" ")

	// the installer itself is fetched without credentials
	req := httptest.NewRequest(http.MethodGet, "/pairing/"+resp.PairingCode, nil)
	req.Header.Set("User-Agent", "curl/8.0")
	installer := httptest.NewRecorder()
	al.router.ServeHTTP(installer, req)
	assert.Equal(t, http.StatusOK, installer.Code)
	assert.Contains(t, installer.Body.String(), `CLIENT_ID="client1"`)
}

func TestPairingRoutesDoNotConflictWithDocRoot(t *testing.T) {
	docRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(docRoot, "index.html"), []byte("<html>UI</html>"), 0600))
	al := newPairingTestListener(t, true, docRoot)

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		al.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	rec := get("/pairing/update")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "templates/linux/update.sh")

	rec = get("/pairing/abcdefghijkmnopqrstu")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Body.String(), "<html>", "unknown codes are not answered by the UI")
	assert.True(t, strings.HasPrefix(rec.Body.String(), "#"))

	rec = get("/dashboard/clients")
	assert.Equal(t, http.StatusOK, rec.Code, "UI history fallback still works")
	assert.Contains(t, rec.Body.String(), "UI")

	rec = get("/")
	assert.Contains(t, rec.Body.String(), "UI")

	// unrelated deeper paths fall through to the file server
	rec = get("/pairing/a/b")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Body.String(), "BEGINNING")
}

func TestPairingDisabledServesNothing(t *testing.T) {
	al := &APIListener{
		insecureForTests: true,
		Server:           &Server{config: &chconfig.Config{Server: chconfig.ServerConfig{PairingURL: "https://pairing.example.com"}}},
	}
	al.initRouter()
	rec := httptest.NewRecorder()
	al.router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pairing/update", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "https://pairing.example.com", al.pairingURL(httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)),
		"external pairing_url is kept")
}

func TestPairingURLReportedByStatus(t *testing.T) {
	al := newPairingTestListener(t, true, "")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	assert.Equal(t, "https://rport.example.com/pairing", al.pairingURL(req), "built-in replaces [server] pairing_url")

	al.config.Pairing.URL = ""
	al.pairing = nil
	al.initRouter()
	req = httptest.NewRequest(http.MethodGet, "https://rport.lan:8443/api/v1/status", nil)
	assert.Equal(t, "https://rport.lan:8443/pairing", al.pairingURL(req), "derived from the request without a configured URL")
}
