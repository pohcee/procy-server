package chserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/oidc/oidcfake"
	"github.com/pohcee/procy-server/share/files"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/tenant"
)

// oidcFixture is a standalone server (SQLite, one static user) with [oidc] on a fake provider.
type oidcFixture struct {
	base string
	fake *oidcfake.Server
	srv  *Server
}

const oidcTestUser = "ann@example.com"
const oidcTestPassword = "a-long-enough-password-1"

func startOIDCServer(t *testing.T, domains ...string) *oidcFixture {
	t.Helper()
	fake := oidcfake.New(t, "procy-client", "procy-secret")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	base := "http://" + l.Addr().String()

	cfg := storageTestConfig(t)
	cfg.API.Auth = oidcTestUser + ":" + oidcTestPassword
	cfg.Logging.LogLevel = logger.LogLevelError
	cfg.API.MaxRequestBytes = 1 << 20
	cfg.API.JWTSecret = "a-jwt-secret-for-the-oidc-tests"
	cfg.OIDC.Enabled = true
	cfg.OIDC.Issuer = fake.URL
	cfg.OIDC.ClientID = fake.ClientID
	cfg.OIDC.ClientSecret = fake.ClientSecret
	cfg.OIDC.RedirectURL = base + "/api/v1/oidc/callback"
	cfg.OIDC.UIURL = base
	cfg.OIDC.ButtonLabel = "Sign in with Google"
	cfg.OIDC.AllowedDomains = domains

	s, err := NewServer(context.Background(), cfg, &ServerOpts{FilesAPI: files.NewFileSystem()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	hs := &httptest.Server{Listener: l, Config: &http.Server{Handler: s.apiListener.router, ReadHeaderTimeout: time.Minute}}
	hs.Start()
	t.Cleanup(hs.Close)
	return &oidcFixture{base: base, fake: fake, srv: s}
}

func (f *oidcFixture) startURL(returnTo string) string {
	u := f.base + "/api/v1/oidc/login"
	if returnTo != "" {
		u += "?return_to=" + url.QueryEscape(returnTo)
	}
	return u
}

// oidcCall sends a JSON request and returns the status and the decoded body.
func oidcCall(t *testing.T, method, u, token string, body any) (int, map[string]any) {
	t.Helper()
	return oidcCallWith(t, http.DefaultClient, method, u, token, body)
}

// oidcCallWith is oidcCall with the client (a browser's cookies) c.
func oidcCallWith(t *testing.T, c *http.Client, method, u, token string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rd)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	out["_raw"] = string(raw)
	return resp.StatusCode, out
}

func TestOIDCStandaloneHappyPath(t *testing.T) {
	f := startOIDCServer(t)
	f.fake.SignInAs(oidcfake.User{Subject: "google-ann", Email: "Ann@Example.com", EmailVerified: true})
	rec := &fakeLoginRecorder{}
	f.srv.apiListener.loginRecorder = rec

	// the login page learns about the button; no secrets
	code, out := oidcCall(t, http.MethodGet, f.base+"/api/v1/auth/provider", "", nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, map[string]any{"enabled": true, "button_label": "Sign in with Google", "login_url": "/api/v1/oidc/login"},
		out["data"].(map[string]any)["oidc"])
	assert.NotContains(t, out["_raw"], "procy-secret")

	b := oidcfake.NewBrowser(t)
	authURL := b.Start(t, f.startURL("/clients?x=1"))
	assert.True(t, strings.HasPrefix(authURL, f.fake.URL+"/authorize?"), authURL)
	ui, frag := b.Callback(t, b.Authorize(t, authURL))
	assert.Equal(t, f.base+"/auth/oidc", ui)
	assert.Equal(t, "/clients?x=1", frag.Get("return_to"))
	require.NotEmpty(t, frag.Get("code"), frag)

	// login CSRF: another browser can't trade the code
	code, out = oidcCall(t, http.MethodPost, f.base+"/api/v1/oidc/token", "", map[string]string{"code": frag.Get("code")})
	assert.Equal(t, http.StatusUnauthorized, code, out["_raw"])

	code, out = oidcCallWith(t, b.Client, http.MethodPost, f.base+"/api/v1/oidc/token", "", map[string]string{"code": frag.Get("code")})
	require.Equal(t, http.StatusOK, code, out["_raw"])
	data := out["data"].(map[string]any)
	token, _ := data["token"].(string)
	require.NotEmpty(t, token)
	assert.Nil(t, data["two_fa"], "no TOTP after an OIDC sign-in")
	rec.mu.Lock()
	assert.Equal(t, []string{"default/" + oidcTestUser}, rec.logins, "the OIDC sign-in is recorded")
	rec.mu.Unlock()

	code, out = oidcCall(t, http.MethodGet, f.base+"/api/v1/me", token, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	me := out["data"].(map[string]any)
	assert.Equal(t, oidcTestUser, me["username"])
	assert.Equal(t, "oidc", me["auth_method"])
	assert.Equal(t, true, me["oidc_linked"])
	assert.NotContains(t, me, "sign_in", "standalone stores no sign-in policy")

	code, out = oidcCall(t, http.MethodGet, f.base+"/api/v1/status", token, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, true, out["data"].(map[string]any)["oidc_enabled"])

	// the code is used once
	code, out = oidcCallWith(t, b.Client, http.MethodPost, f.base+"/api/v1/oidc/token", "", map[string]string{"code": frag.Get("code")})
	assert.Equal(t, http.StatusUnauthorized, code, out["_raw"])
	code, out = oidcCall(t, http.MethodPost, f.base+"/api/v1/oidc/token", "", map[string]string{"code": "guessed"})
	assert.Equal(t, http.StatusUnauthorized, code, out["_raw"])

	// the password still works for this user (both)
	req, err := http.NewRequest(http.MethodGet, f.base+"/api/v1/login", nil)
	require.NoError(t, err)
	req.SetBasicAuth(oidcTestUser, oidcTestPassword)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestOIDCStandaloneRefusals(t *testing.T) {
	f := startOIDCServer(t, "example.com")
	signIn := func(t *testing.T, u oidcfake.User) url.Values {
		t.Helper()
		f.fake.SignInAs(u)
		return oidcfake.NewBrowser(t).SignIn(t, f.startURL(""))
	}
	ann := oidcfake.User{Subject: "google-ann", Email: oidcTestUser, EmailVerified: true}

	t.Run("unverified email", func(t *testing.T) {
		u := ann
		u.EmailVerified = false
		assert.Equal(t, "email_unverified", signIn(t, u).Get("error"))
	})
	t.Run("domain not allowed", func(t *testing.T) {
		assert.Equal(t, "domain_not_allowed", signIn(t, oidcfake.User{Subject: "x", Email: "ann@other.com", EmailVerified: true}).Get("error"),
			"not Google: the email domain counts")
		assert.Equal(t, "domain_not_allowed", signIn(t, oidcfake.User{Subject: "x", Email: oidcTestUser, EmailVerified: true, HostedDomain: "other.com"}).Get("error"),
			"the Workspace domain decides")
	})
	t.Run("unknown email creates no account", func(t *testing.T) {
		frag := signIn(t, oidcfake.User{Subject: "stranger", Email: "nobody@example.com", EmailVerified: true})
		assert.Equal(t, "unknown_account", frag.Get("error"))
		assert.Empty(t, frag.Get("code"))
		all, err := f.srv.apiListener.userService.GetAll(tenant.WithID(context.Background(), tenant.Default))
		require.NoError(t, err)
		assert.Len(t, all, 1)
	})
	t.Run("subject pinning", func(t *testing.T) {
		frag := signIn(t, ann)
		require.NotEmpty(t, frag.Get("code"), frag)
		// another Google account that claims ann's address
		frag = signIn(t, oidcfake.User{Subject: "google-impostor", Email: oidcTestUser, EmailVerified: true})
		assert.Equal(t, "account_mismatch", frag.Get("error"))
		// ann's own account, after its Google email changed, is still ann
		frag = signIn(t, oidcfake.User{Subject: "google-ann", Email: "ann.renamed@example.com", EmailVerified: true})
		assert.NotEmpty(t, frag.Get("code"), frag)
	})
	t.Run("replayed state", func(t *testing.T) {
		f.fake.SignInAs(ann)
		b := oidcfake.NewBrowser(t)
		cb := b.Authorize(t, b.Start(t, f.startURL("")))
		_, frag := b.Callback(t, cb)
		require.NotEmpty(t, frag.Get("code"))
		_, frag = b.Callback(t, cb)
		assert.Equal(t, "invalid_state", frag.Get("error"))
	})
	t.Run("unknown state", func(t *testing.T) {
		b := oidcfake.NewBrowser(t)
		_, frag := b.Callback(t, f.base+"/api/v1/oidc/callback?state=made-up&code=x")
		assert.Equal(t, "invalid_state", frag.Get("error"))
	})
	t.Run("callback in another browser", func(t *testing.T) {
		f.fake.SignInAs(ann)
		victim := oidcfake.NewBrowser(t)
		cb := oidcfake.NewBrowser(t).Authorize(t, oidcfake.NewBrowser(t).Start(t, f.startURL("")))
		_, frag := victim.Callback(t, cb)
		assert.Equal(t, "invalid_state", frag.Get("error"), "login CSRF: the binding cookie is missing")
	})
	t.Run("wrong nonce", func(t *testing.T) {
		f.fake.NonceOverride = "replayed-nonce"
		t.Cleanup(func() { f.fake.NonceOverride = "" })
		assert.Equal(t, "invalid_state", signIn(t, ann).Get("error"))
	})
	t.Run("canceled at the provider", func(t *testing.T) {
		f.fake.Deny = true
		t.Cleanup(func() { f.fake.Deny = false })
		assert.Equal(t, "access_denied", signIn(t, ann).Get("error"))
	})
	t.Run("return_to stays on the UI", func(t *testing.T) {
		f.fake.SignInAs(ann)
		for _, evil := range []string{"https://evil.example/x", "//evil.example", "/\\evil.example"} {
			frag := oidcfake.NewBrowser(t).SignIn(t, f.startURL(evil))
			require.NotEmpty(t, frag.Get("code"), frag)
			assert.Empty(t, frag.Get("return_to"), evil)
		}
	})
}

// A deleted user's pin doesn't carry over to a new user of the same name.
func TestOIDCStandaloneUnlinkOnDelete(t *testing.T) {
	f := startOIDCServer(t)
	ctx := tenant.WithID(context.Background(), tenant.Default)
	ann := oidcfake.User{Subject: "google-ann", Email: oidcTestUser, EmailVerified: true}
	f.fake.SignInAs(ann)
	require.NotEmpty(t, oidcfake.NewBrowser(t).SignIn(t, f.startURL("")).Get("code"))
	linked, err := f.srv.apiListener.oidcLogin.store.Linked(ctx, []string{oidcTestUser})
	require.NoError(t, err)
	require.True(t, linked[oidcTestUser])
	f.srv.apiListener.unlinkStandalone(ctx, oidcTestUser)
	linked, err = f.srv.apiListener.oidcLogin.store.Linked(ctx, []string{oidcTestUser})
	require.NoError(t, err)
	assert.False(t, linked[oidcTestUser])
}

func TestSafeReturnTo(t *testing.T) {
	for in, want := range map[string]string{
		"/dashboard": "/dashboard", "/a?b=c": "/a?b=c", "": "", "dashboard": "", "//evil": "", "/\\evil": "",
		"https://evil": "", "/x\r\nSet-Cookie: a": "", "/" + strings.Repeat("a", 600): "",
	} {
		assert.Equal(t, want, safeReturnTo(in), in)
	}
}

func TestVerifyPasswordRefusesEmptyHash(t *testing.T) {
	assert.False(t, verifyPassword("", ""), "an account without a password never verifies")
	assert.False(t, verifyPassword("", "x"))
	assert.True(t, verifyPassword("plain", "plain"))
}
