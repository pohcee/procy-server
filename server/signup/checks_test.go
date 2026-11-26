package signup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/chconfig"
)

func TestSlugAndOrganizationID(t *testing.T) {
	for in, want := range map[string]string{
		"Dana's Widgets, Inc.":    "dana-s-widgets-inc",
		"  ---  ":                 "org",
		"Ünïcode only":            "n-code-only",
		"日本":                      "org",
		strings.Repeat("ab ", 40): strings.Trim(strings.Repeat("ab-", 14)[:40], "-"),
	} {
		assert.Equal(t, want, slug(in), in)
		assert.LessOrEqual(t, len(slug(in)), maxSlug)
	}
	id, err := organizationID("Acme")
	require.NoError(t, err)
	assert.Regexp(t, `^acme-[a-z0-9]{6}$`, id)
}

func TestNormalizeEmail(t *testing.T) {
	a, d, err := normalizeEmail("  Dana@Example.COM ")
	require.NoError(t, err)
	assert.Equal(t, "dana@example.com", a)
	assert.Equal(t, "example.com", d)
	for _, bad := range []string{"", "x", "x@localhost", "a b@example.com", strings.Repeat("a", 250) + "@example.com"} {
		_, _, err := normalizeEmail(bad)
		assert.ErrorIs(t, err, ErrInvalid, bad)
	}
}

func TestPasswordRules(t *testing.T) {
	p := PasswordRules{MinLength: 12, MinScore: 3}
	assert.ErrorIs(t, p.check("short", "u@x.io"), ErrInvalid)
	assert.ErrorIs(t, p.check("short", "u@x.io"), ErrPassword)
	assert.NotErrorIs(t, ErrToken, ErrPassword)
	assert.ErrorIs(t, p.check("aaaaaaaaaaaaaaaa", "u@x.io"), ErrInvalid, "guessable")
	assert.ErrorIs(t, p.check(strings.Repeat("Zq9!", 20), "u@x.io"), ErrInvalid, "too long for bcrypt")
	assert.NoError(t, p.check("a much longer passphrase 1984", "u@x.io"))
	assert.NoError(t, PasswordRules{MinLength: 4, MinScore: -1}.check("aaaa", "u"))
}

func TestDomains(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	require.NoError(t, os.WriteFile(blocked, []byte("# list\nMailinator.com\n@trash.example  # comment\n\n"), 0600))
	d, err := LoadDomains(blocked, "")
	require.NoError(t, err)
	ctx := context.Background()
	for _, dom := range []string{"mailinator.com", "eu.mailinator.com", "trash.example"} {
		assert.ErrorIs(t, d.Check(ctx, dom), ErrDomain, dom)
	}
	assert.NoError(t, d.Check(ctx, "notmailinator.com"))

	allow := NewDomains(nil, []string{"corp.example"})
	assert.NoError(t, allow.Check(ctx, "corp.example"))
	assert.NoError(t, allow.Check(ctx, "eu.corp.example"))
	assert.ErrorIs(t, allow.Check(ctx, "gmail.com"), ErrDomain)
	var none *Domains
	assert.NoError(t, none.Check(ctx, "anything.example"))
	_, err = LoadDomains(filepath.Join(dir, "missing"), "")
	assert.Error(t, err)
}

func TestTokens(t *testing.T) {
	tok, hash, err := newToken()
	require.NoError(t, err)
	assert.Len(t, tok, 43)
	assert.Equal(t, hash, hashToken(tok))
	assert.NotContains(t, hash, tok)
	tok2, _, _ := newToken()
	assert.NotEqual(t, tok, tok2)
}

func TestSiteVerify(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		got = map[string]string{"secret": r.Form.Get("secret"), "response": r.Form.Get("response"), "remoteip": r.Form.Get("remoteip")}
		if r.Form.Get("response") == "good" {
			_, _ = w.Write([]byte(`{"success": true}`))
			return
		}
		_, _ = w.Write([]byte(`{"success": false, "error-codes": ["invalid-input-response"]}`))
	}))
	defer srv.Close()
	v := &SiteVerify{URL: srv.URL, Secret: "s3cret", Client: srv.Client()}
	ctx := context.Background()
	require.NoError(t, v.Verify(ctx, "good", "192.0.2.7"))
	assert.Equal(t, map[string]string{"secret": "s3cret", "response": "good", "remoteip": "192.0.2.7"}, got)
	assert.ErrorIs(t, v.Verify(ctx, "bad", ""), ErrCaptcha)
	assert.ErrorIs(t, v.Verify(ctx, "", ""), ErrCaptcha)
	down := &SiteVerify{URL: "http://127.0.0.1:1", Secret: "s"}
	assert.ErrorIs(t, down.Verify(ctx, "good", ""), ErrCaptcha, "fails closed")
}

func TestNewCaptcha(t *testing.T) {
	c, err := NewCaptcha(chconfig.SignupConfig{})
	require.NoError(t, err)
	assert.Nil(t, c)
	p := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(p, []byte("  abc\n"), 0600))
	c, err = NewCaptcha(chconfig.SignupConfig{Captcha: chconfig.SignupCaptchaHCaptcha, CaptchaSecretFile: p})
	require.NoError(t, err)
	assert.Equal(t, HCaptchaVerifyURL, c.(*SiteVerify).URL)
	assert.Equal(t, "abc", c.(*SiteVerify).Secret)
	require.NoError(t, os.WriteFile(p, []byte("\n"), 0600))
	_, err = NewCaptcha(chconfig.SignupConfig{Captcha: chconfig.SignupCaptchaTurnstile, CaptchaSecretFile: p})
	assert.Error(t, err, "empty secret")
}
