package pairing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/share/logger"
)

const (
	linuxUA   = "curl/7.79.1"
	windowsUA = "Mozilla/5.0 (Windows NT; Windows NT 10.0; en-US) WindowsPowerShell/5.1.20348.1"
	publicURL = "https://rport.example.com"
	dlURL     = "https://rport.example.com/downloads"
)

var testDeposit = Deposit{
	ConnectURL:  "https://rport.example.com:8080",
	Fingerprint: "2a:c4:79:04:80:ba:7c:60:05:e5:2c:49:6d:74:56:24",
	ClientID:    "client1",
	Password:    "foobaz",
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func testLogger() *logger.Logger {
	return logger.NewLogger("pairing-test", logger.LogOutput{File: os.Stdout}, logger.LogLevelError)
}

func newTestService(opts Options) (*Service, *clock, *mux.Router) {
	c := &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	s := newService(opts, testLogger(), c.now)
	r := mux.NewRouter()
	r.HandleFunc(PathPrefix, func(w http.ResponseWriter, r *http.Request) { s.HandleDeposit(w, r, "admin") }).Methods(http.MethodPost)
	r.HandleFunc(PathPrefix+"/update", s.HandleUpdate).Methods(http.MethodGet)
	r.HandleFunc(PathPrefix+"/{"+CodeVar+":[0-9A-Za-z]{1,64}}", s.HandleInstaller).Methods(http.MethodGet)
	return s, c, r
}

func do(r http.Handler, method, target, ua string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func deposit(t *testing.T, r http.Handler, d Deposit) Response {
	t.Helper()
	payload, err := json.Marshal(d)
	require.NoError(t, err)
	rec := do(r, http.MethodPost, PathPrefix, "", bytes.NewReader(payload), "application/json")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp Response
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func TestGeneratedCodesAreLongRandomAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		code, err := generateCode()
		require.NoError(t, err)
		require.Len(t, code, CodeLength)
		for _, ch := range code {
			require.Contains(t, codeAlphabet, string(ch))
		}
		require.False(t, seen[code], "duplicate code")
		seen[code] = true
	}
}

func TestDepositReturnsInstallersOnPublicURL(t *testing.T) {
	_, c, r := newTestService(Options{PublicURL: publicURL + "/", DownloadsURL: dlURL})
	resp := deposit(t, r, testDeposit)

	assert.Len(t, resp.PairingCode, CodeLength)
	assert.Equal(t, c.now().Add(time.Hour), resp.Expires, "default TTL is 1 hour")
	installer := publicURL + "/pairing/" + resp.PairingCode
	assert.Equal(t, "curl "+installer+" > rport-installer.sh\nsudo sh rport-installer.sh", resp.Installers.Linux)
	assert.Contains(t, resp.Installers.Windows, `$url="`+installer+`"`)
	assert.Contains(t, resp.Installers.Windows, `Invoke-WebRequest -Uri $url -OutFile "rport-installer.ps1"`)
}

func TestDepositMultipartForm(t *testing.T) {
	_, _, r := newTestService(Options{PublicURL: publicURL})
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	for k, v := range map[string]string{
		"connect_url": testDeposit.ConnectURL, "fingerprint": testDeposit.Fingerprint,
		"client_id": testDeposit.ClientID, "password": testDeposit.Password,
	} {
		require.NoError(t, w.WriteField(k, v))
	}
	require.NoError(t, w.Close())
	rec := do(r, http.MethodPost, PathPrefix, "", body, w.FormDataContentType())
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestDepositValidation(t *testing.T) {
	_, _, r := newTestService(Options{PublicURL: publicURL})
	mod := func(f func(d *Deposit)) Deposit { d := testDeposit; f(&d); return d }
	for name, d := range map[string]Deposit{
		"missing password":      mod(func(d *Deposit) { d.Password = "" }),
		"missing client id":     mod(func(d *Deposit) { d.ClientID = "" }),
		"bad connect url":       mod(func(d *Deposit) { d.ConnectURL = "rport.example.com" }),
		"ftp connect url":       mod(func(d *Deposit) { d.ConnectURL = "ftp://rport.example.com" }),
		"short fingerprint":     mod(func(d *Deposit) { d.Fingerprint = "2a:c4" }),
		"newline in password":   mod(func(d *Deposit) { d.Password = "a\nrm -rf /" }),
		"overlong client id":    mod(func(d *Deposit) { d.ClientID = strings.Repeat("a", maxFieldLen+1) }),
		"non-hex fingerprint47": mod(func(d *Deposit) { d.Fingerprint = "zz:c4:79:04:80:ba:7c:60:05:e5:2c:49:6d:74:56:24" }),
	} {
		payload, _ := json.Marshal(d)
		rec := do(r, http.MethodPost, PathPrefix, "", bytes.NewReader(payload), "application/json")
		assert.Equal(t, http.StatusBadRequest, rec.Code, name)
	}
	rec := do(r, http.MethodPost, PathPrefix, "", strings.NewReader("{"), "application/json")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestInstallerOSSelectionAndContent(t *testing.T) {
	_, _, r := newTestService(Options{PublicURL: publicURL, DownloadsURL: dlURL, AllowCodeReuse: true})
	code := deposit(t, r, testDeposit).PairingCode

	rec := do(r, http.MethodGet, "/pairing/"+code, linuxUA, nil, "")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Equal(t, `attachment; filename="rport-installer.sh"`, rec.Header().Get("Content-Disposition"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Contains(t, body, "BEGINNING of templates/linux/install.sh")
	assert.Contains(t, body, `CLIENT_ID="client1"`)
	assert.Contains(t, body, `PASSWORD="foobaz"`)
	assert.Contains(t, body, `CONNECT_URL="https://rport.example.com:8080"`)
	assert.Contains(t, body, `RPORT_DOWNLOAD_BASE="`+dlURL+`"`)
	assert.Contains(t, body, `RPORT_PAIRING_URL="`+publicURL+`/pairing"`)
	assert.Contains(t, body, `DL_URL="${RPORT_DOWNLOAD_BASE}/rport/${RELEASE}/${DL_FILE}"`)
	assert.Contains(t, body, `verify_rport_download rport.tar.gz "${DL_FILE}"`)

	rec = do(r, http.MethodGet, "/pairing/"+code, windowsUA, nil, "")
	require.Equal(t, http.StatusOK, rec.Code)
	body = rec.Body.String()
	assert.Equal(t, `attachment; filename="rport-installer.ps1"`, rec.Header().Get("Content-Disposition"))
	assert.Contains(t, body, "function Expand-Zip")
	assert.Contains(t, body, `$client_id = "client1"`)
	assert.Contains(t, body, `$RportDownloadBase = "`+dlURL+`"`)
	assert.Contains(t, body, `$RportPairingUrl = "`+publicURL+`/pairing"`)
	assert.Contains(t, body, `rport_windows_x86_64.zip`)
	assert.Contains(t, body, `Confirm-Checksum`)
}

func TestUpdateOSSelection(t *testing.T) {
	_, _, r := newTestService(Options{PublicURL: publicURL, DownloadsURL: dlURL})
	for _, tc := range []struct{ ua, file, keyword, base string }{
		{linuxUA, "rport-update.sh", "BEGINNING of templates/linux/update.sh", `RPORT_DOWNLOAD_BASE="` + dlURL + `"`},
		{"", "rport-update.sh", "BEGINNING of templates/linux/update.sh", `version_newer "$LATEST_VERSION" "$CURRENT_VERSION"`},
		{"Wget/1.21", "rport-update.sh", "BEGINNING of templates/linux/update.sh", `start_supervisor`},
		{windowsUA, "rport-update.ps1", "BEGINNING of templates/windows/update.ps1", `$RportDownloadBase = "` + dlURL + `"`},
		{"Mozilla/5.0 (Windows NT 10.0; Microsoft Windows 10.0.22631; en-US) PowerShell/7.4.1", "rport-update.ps1", "Test-NewerVersion -Latest $latest -Current $gt", "Confirm-Checksum"},
	} {
		rec := do(r, http.MethodGet, "/pairing/update", tc.ua, nil, "")
		require.Equal(t, http.StatusOK, rec.Code, tc.ua)
		assert.Equal(t, fmt.Sprintf("attachment; filename=%q", tc.file), rec.Header().Get("Content-Disposition"), tc.ua)
		assert.Contains(t, rec.Body.String(), tc.keyword, tc.ua)
		assert.Contains(t, rec.Body.String(), tc.base, tc.ua)
		assert.NotContains(t, rec.Body.String(), "PASSWORD=", tc.ua)
	}
}

func TestCodeIsSingleUseByDefault(t *testing.T) {
	s, _, r := newTestService(Options{PublicURL: publicURL})
	code := deposit(t, r, testDeposit).PairingCode
	assert.Equal(t, http.StatusOK, do(r, http.MethodGet, "/pairing/"+code, linuxUA, nil, "").Code)
	rec := do(r, http.MethodGet, "/pairing/"+code, linuxUA, nil, "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Body.String(), code, "the code is not echoed")
	assert.NotContains(t, rec.Body.String(), "foobaz")
	assert.Equal(t, 0, s.codes.len())
}

func TestCodeExpiry(t *testing.T) {
	for _, tc := range []struct {
		ttl, valid time.Duration
	}{
		{0, time.Hour}, // default
		{10 * time.Minute, 10 * time.Minute},
	} {
		_, c, r := newTestService(Options{PublicURL: publicURL, CodeTTL: tc.ttl, AllowCodeReuse: true})
		resp := deposit(t, r, testDeposit)
		assert.Equal(t, c.now().Add(tc.valid), resp.Expires)

		c.add(tc.valid - time.Second)
		assert.Equal(t, http.StatusOK, do(r, http.MethodGet, "/pairing/"+resp.PairingCode, linuxUA, nil, "").Code, "still valid")
		assert.Equal(t, http.StatusOK, do(r, http.MethodGet, "/pairing/"+resp.PairingCode, linuxUA, nil, "").Code, "reuse allowed")
		c.add(time.Second)
		assert.Equal(t, http.StatusNotFound, do(r, http.MethodGet, "/pairing/"+resp.PairingCode, linuxUA, nil, "").Code, "expired")
	}
}

func TestExpiredCodesArePurged(t *testing.T) {
	s, c, r := newTestService(Options{PublicURL: publicURL})
	deposit(t, r, testDeposit)
	deposit(t, r, testDeposit)
	assert.Equal(t, 2, s.codes.len())
	c.add(2 * time.Hour)
	deposit(t, r, testDeposit)
	assert.Equal(t, 1, s.codes.len())
}

func TestFailedLookupsAreRateLimited(t *testing.T) {
	_, c, r := newTestService(Options{PublicURL: publicURL})
	code := deposit(t, r, testDeposit).PairingCode
	for i := 0; i < maxFailedLookups; i++ {
		assert.Equal(t, http.StatusNotFound, do(r, http.MethodGet, "/pairing/wrongcode", linuxUA, nil, "").Code)
	}
	assert.Equal(t, http.StatusTooManyRequests, do(r, http.MethodGet, "/pairing/"+code, linuxUA, nil, "").Code, "blocked even for a valid code")

	other := httptest.NewRequest(http.MethodGet, "/pairing/"+code, nil)
	other.RemoteAddr = "198.51.100.7:1234"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, other)
	assert.Equal(t, http.StatusOK, rec.Code, "other IPs are not affected")

	c.add(failedLookupsWindow)
	assert.Equal(t, http.StatusNotFound, do(r, http.MethodGet, "/pairing/wrongcode", linuxUA, nil, "").Code, "window over")
}

func TestPublicURLFromRequestWhenNotConfigured(t *testing.T) {
	s, _, r := newTestService(Options{})
	req := httptest.NewRequest(http.MethodGet, "http://rport.internal:3000/pairing/update", nil)
	assert.Equal(t, "http://rport.internal:3000/pairing", s.URL(req))
	req.Header.Set("X-Forwarded-Proto", "https")
	assert.Equal(t, "https://rport.internal:3000/pairing", s.URL(req))

	rec := do(r, http.MethodGet, "http://rport.lan/pairing/update", linuxUA, nil, "")
	assert.Contains(t, rec.Body.String(), `RPORT_PAIRING_URL="http://rport.lan/pairing"`)
}

func TestMissingDownloadBaseFailsClearly(t *testing.T) {
	body, _, err := renderInstaller(osLinux, testDeposit, scriptSettings{})
	require.NoError(t, err)
	assert.Contains(t, string(body), `RPORT_DOWNLOAD_BASE=""`)
	assert.Contains(t, string(body), "No downloads URL configured (RPORT_DOWNLOAD_BASE)")
}

// Generated scripts must only download from and point to the configured sites:
// no upstream download, package repository, pairing or support addresses.
func TestScriptsHaveNoUpstreamAddresses(t *testing.T) {
	upstream := []string{"openrport.io", "rport.io", "github.com/openrport/openrport/releases", "discussions", "pairing.openrport"}
	cfg := scriptSettings{DownloadsURL: dlURL, PairingURL: publicURL + "/pairing"}
	for _, os := range []string{osLinux, osWindows} {
		installer, _, err := renderInstaller(os, testDeposit, cfg)
		require.NoError(t, err)
		update, _ := renderUpdate(os, cfg)
		for name, body := range map[string][]byte{"installer": installer, "update": update} {
			for _, line := range strings.Split(string(body), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "##") {
					continue // license/attribution header
				}
				for _, u := range upstream {
					assert.NotContains(t, line, u, "%s %s", os, name)
				}
			}
		}
	}
}

var nastyValues = []string{
	`p"; rm -rf / #`,
	`$(id)`,
	"`id`",
	`\"$HOME\\`,
	"ü€ “smart” „quotes”",
	`'single' ${PATH}`,
}

func TestEscaping(t *testing.T) {
	assert.Equal(t, `a\"b\$c\\d\`+"`e", EscapeForBash("a\"b$c\\d`e"))
	assert.Equal(t, "a`\"b`$c``d`“e`”f`„g\\h", EscapeForPowerShell("a\"b$c`d“e”f„g\\h"))

	for _, v := range nastyValues {
		d := testDeposit
		d.ClientID, d.Password = v, v
		body, _, err := renderInstaller(osLinux, d, scriptSettings{DownloadsURL: v, PairingURL: v})
		require.NoError(t, err)
		assert.Contains(t, string(body), `PASSWORD="`+EscapeForBash(v)+`"`)
		assert.Contains(t, string(body), `RPORT_DOWNLOAD_BASE="`+EscapeForBash(v)+`"`)
		body, _, err = renderInstaller(osWindows, d, scriptSettings{DownloadsURL: v})
		require.NoError(t, err)
		assert.Contains(t, string(body), `$password = "`+EscapeForPowerShell(v)+`"`)
	}
}

// Run the rendered variable assignments through a real shell and check the values
// come out unchanged, i.e. nothing in them is interpreted.
func TestEscapingRoundTripsThroughShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, v := range nastyValues {
		d := testDeposit
		d.ClientID, d.Password = v, v
		s := &script{}
		require.NoError(t, s.render("vars", linuxVarsTpl, d.escaped(EscapeForBash)))
		s.downloadBase(osLinux, scriptSettings{DownloadsURL: v, PairingURL: v})
		s.buf.WriteString(`printf '%s\n%s\n%s\n%s' "$CLIENT_ID" "$PASSWORD" "$RPORT_DOWNLOAD_BASE" "$RPORT_PAIRING_URL"`)
		out, err := exec.Command(sh, "-c", s.buf.String()).CombinedOutput()
		require.NoError(t, err, string(out))
		assert.Equal(t, strings.Join([]string{v, v, v, v}, "\n"), string(out))
	}
}

func TestEscapingRoundTripsThroughPowerShell(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("no pwsh")
	}
	for _, v := range nastyValues {
		d := testDeposit
		d.ClientID, d.Password = v, v
		s := &script{}
		require.NoError(t, s.render("vars", windowsVarsTpl, d.escaped(EscapeForPowerShell)))
		s.buf.WriteString("[Console]::Out.Write($password)")
		out, err := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", s.buf.String()).CombinedOutput()
		require.NoError(t, err, string(out))
		assert.Equal(t, v, string(out))
	}
}
