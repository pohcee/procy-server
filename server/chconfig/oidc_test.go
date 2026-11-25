package chconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOIDCConfig(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secret, []byte("s3cret\n"), 0600))
	empty := filepath.Join(t.TempDir(), "empty")
	require.NoError(t, os.WriteFile(empty, nil, 0600))
	good := func() OIDCConfig {
		return OIDCConfig{Enabled: true, ClientID: "cid", ClientSecretFile: secret, RedirectURL: "https://procy.sh/api/v1/oidc/callback"}
	}

	c := &Config{OIDC: good()}
	require.NoError(t, c.parseAndValidateOIDC())
	assert.Equal(t, DefaultOIDCIssuer, c.OIDC.Issuer)
	assert.Equal(t, DefaultOIDCButtonLabel, c.OIDC.ButtonLabel)
	assert.Equal(t, "s3cret", c.OIDC.ClientSecret)
	assert.Equal(t, "https://procy.sh", c.OIDC.UIURL)
	assert.Equal(t, "https://procy.sh/auth/oidc", c.OIDC.UIReturnURL())

	off := &Config{OIDC: OIDCConfig{RedirectURL: "nonsense"}}
	require.NoError(t, off.parseAndValidateOIDC(), "disabled: not validated")

	for name, mutate := range map[string]func(o *OIDCConfig){
		"no client id":       func(o *OIDCConfig) { o.ClientID = "" },
		"no secret file":     func(o *OIDCConfig) { o.ClientSecretFile = "" },
		"missing secret":     func(o *OIDCConfig) { o.ClientSecretFile = filepath.Join(t.TempDir(), "nope") },
		"empty secret":       func(o *OIDCConfig) { o.ClientSecretFile = empty },
		"http redirect":      func(o *OIDCConfig) { o.RedirectURL = "http://procy.sh/api/v1/oidc/callback" },
		"wrong path":         func(o *OIDCConfig) { o.RedirectURL = "https://procy.sh/callback" },
		"http issuer":        func(o *OIDCConfig) { o.Issuer = "http://accounts.example.com" },
		"bad domain":         func(o *OIDCConfig) { o.AllowedDomains = []string{"a@b.com"} },
		"http ui url remote": func(o *OIDCConfig) { o.UIURL = "http://procy.sh" },
	} {
		t.Run(name, func(t *testing.T) {
			c := &Config{OIDC: good()}
			mutate(&c.OIDC)
			assert.Error(t, c.parseAndValidateOIDC())
		})
	}

	local := &Config{OIDC: good()}
	local.OIDC.Issuer = "http://127.0.0.1:1234"
	local.OIDC.RedirectURL = "http://localhost:3000/api/v1/oidc/callback"
	require.NoError(t, local.parseAndValidateOIDC(), "loopback http is allowed")
}

func TestOIDCDomainAllowed(t *testing.T) {
	o := OIDCConfig{}
	assert.True(t, o.DomainAllowed("a@gmail.com", ""))
	o.AllowedDomains = []string{"Example.com"}
	assert.True(t, o.DomainAllowed("a@example.com", ""))
	assert.True(t, o.DomainAllowed("a@other.com", "example.com"), "the Workspace domain decides")
	assert.False(t, o.DomainAllowed("a@example.com", "other.com"))
	assert.False(t, o.DomainAllowed("a@gmail.com", ""))
	assert.False(t, o.DomainAllowed("nodomain", ""))
}
