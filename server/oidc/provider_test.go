package oidc_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/oidc"
	"github.com/openrport/openrport/server/oidc/oidcfake"
)

const redirect = "http://127.0.0.1:1/api/v1/oidc/callback"

func providerFor(fake *oidcfake.Server, domains ...string) *oidc.Provider {
	return oidc.NewProvider(chconfig.OIDCConfig{Enabled: true, Issuer: fake.URL, ClientID: fake.ClientID,
		ClientSecret: fake.ClientSecret, RedirectURL: redirect, AllowedDomains: domains}, nil)
}

// authorize runs the provider's authorization endpoint and returns the code and state of the callback.
func authorize(t *testing.T, authURL string) (code, state string) {
	t.Helper()
	b := oidcfake.NewBrowser(t)
	cb, err := url.Parse(b.Authorize(t, authURL))
	require.NoError(t, err)
	return cb.Query().Get("code"), cb.Query().Get("state")
}

func TestProviderExchange(t *testing.T) {
	fake := oidcfake.New(t, "client-1", "secret-1")
	fake.SignInAs(oidcfake.User{Subject: "sub-1", Email: "Alice@Example.com", EmailVerified: true, HostedDomain: "example.com"})
	p := providerFor(fake, "example.com")

	verifier := oauth2.GenerateVerifier()
	authURL, err := p.AuthURL("state-1", "nonce-1", verifier)
	require.NoError(t, err)
	u, err := url.Parse(authURL)
	require.NoError(t, err)
	q := u.Query()
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.NotEmpty(t, q.Get("code_challenge"))
	assert.Equal(t, "nonce-1", q.Get("nonce"))
	assert.Equal(t, "example.com", q.Get("hd"))
	assert.Equal(t, "openid email profile", q.Get("scope"))

	code, state := authorize(t, authURL)
	assert.Equal(t, "state-1", state)
	id, err := p.Exchange(context.Background(), code, verifier, "nonce-1")
	require.NoError(t, err)
	assert.Equal(t, oidc.Identity{Issuer: fake.URL, Subject: "sub-1", Email: "Alice@Example.com", EmailVerified: true,
		HostedDomain: "example.com"}, *id)

	// a code is redeemed once
	_, err = p.Exchange(context.Background(), code, verifier, "nonce-1")
	assert.Error(t, err)
}

func TestProviderRefusesWrongNonceAndVerifier(t *testing.T) {
	fake := oidcfake.New(t, "client-1", "secret-1")
	fake.SignInAs(oidcfake.User{Subject: "sub-1", Email: "a@example.com", EmailVerified: true})
	p := providerFor(fake)
	verifier := oauth2.GenerateVerifier()

	authURL, err := p.AuthURL("s", "nonce-1", verifier)
	require.NoError(t, err)
	code, _ := authorize(t, authURL)
	_, err = p.Exchange(context.Background(), code, verifier, "another-nonce")
	assert.ErrorIs(t, err, oidc.ErrNonce)

	authURL, err = p.AuthURL("s", "nonce-1", verifier)
	require.NoError(t, err)
	code, _ = authorize(t, authURL)
	_, err = p.Exchange(context.Background(), code, oauth2.GenerateVerifier(), "nonce-1")
	assert.Error(t, err, "a different PKCE verifier")
}

func TestProviderUnverifiedEmail(t *testing.T) {
	fake := oidcfake.New(t, "client-1", "secret-1")
	fake.SignInAs(oidcfake.User{Subject: "sub-1", Email: "a@example.com", EmailVerified: false})
	p := providerFor(fake)
	verifier := oauth2.GenerateVerifier()
	authURL, err := p.AuthURL("s", "n", verifier)
	require.NoError(t, err)
	code, _ := authorize(t, authURL)
	id, err := p.Exchange(context.Background(), code, verifier, "n")
	require.NoError(t, err)
	assert.False(t, id.EmailVerified)
}

func TestProviderUnreachable(t *testing.T) {
	p := oidc.NewProvider(chconfig.OIDCConfig{Issuer: "http://127.0.0.1:1", ClientID: "c", RedirectURL: redirect}, nil)
	_, err := p.AuthURL("s", "n", "v")
	assert.Error(t, err)
}

func TestPolicy(t *testing.T) {
	for _, tc := range []struct {
		p              oidc.Policy
		password, oidc bool
	}{
		{oidc.Policy{}, true, true},
		{oidc.Policy{Method: oidc.MethodBoth}, true, true},
		{oidc.Policy{Method: oidc.MethodPassword}, true, false},
		{oidc.Policy{Method: oidc.MethodOIDC}, false, true},
		{oidc.Policy{RequireOIDC: true}, false, true},
		{oidc.Policy{RequireOIDC: true, Method: oidc.MethodPassword}, false, true},
		{oidc.Policy{System: true}, false, true},
		{oidc.Policy{System: true, Method: oidc.MethodPassword}, false, true},
	} {
		assert.Equal(t, tc.password, tc.p.PasswordAllowed(), "%+v", tc.p)
		assert.Equal(t, tc.oidc, tc.p.OIDCAllowed(), "%+v", tc.p)
	}
	_, err := oidc.ParseMethod("sometimes")
	assert.Error(t, err)
	m, err := oidc.ParseMethod("oidc")
	require.NoError(t, err)
	assert.Equal(t, oidc.MethodOIDC, m)
}
