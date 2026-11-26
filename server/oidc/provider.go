package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/pohcee/procy-server/server/chconfig"
)

// ErrNonce: the ID token does not carry the nonce of the sign-in (a replayed or substituted token).
var ErrNonce = errors.New("oidc: the ID token's nonce does not match the sign-in")

// Provider is the OpenID Connect provider of [oidc]. Its discovery document is fetched on first use (and again
// after a failure), so the server starts even when the provider can't be reached.
type Provider struct {
	cfg    chconfig.OIDCConfig
	client *http.Client

	mu       sync.Mutex
	oauth    *oauth2.Config
	verifier *gooidc.IDTokenVerifier
}

// NewProvider returns the provider of cfg; client nil is a client with a 15 s timeout.
func NewProvider(cfg chconfig.OIDCConfig, client *http.Client) *Provider {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Provider{cfg: cfg, client: client}
}

// clientContext carries the provider's HTTP client for go-oidc and oauth2. It is not the request's context: the
// key set go-oidc keeps fetches keys with it later.
func (p *Provider) clientContext() context.Context {
	return gooidc.ClientContext(context.Background(), p.client)
}

func (p *Provider) discover() (*oauth2.Config, *gooidc.IDTokenVerifier, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.oauth != nil {
		return p.oauth, p.verifier, nil
	}
	prov, err := gooidc.NewProvider(p.clientContext(), p.cfg.Issuer)
	if err != nil {
		return nil, nil, fmt.Errorf("oidc: discovery of %s: %w", p.cfg.Issuer, err)
	}
	p.oauth = &oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		Endpoint:     prov.Endpoint(),
		RedirectURL:  p.cfg.RedirectURL,
		Scopes:       []string{gooidc.ScopeOpenID, "email", "profile"},
	}
	p.verifier = prov.Verifier(&gooidc.Config{ClientID: p.cfg.ClientID})
	return p.oauth, p.verifier, nil
}

// AuthURL returns the provider's authorization URL for a sign-in with state, nonce and the PKCE verifier.
func (p *Provider) AuthURL(state, nonce, verifier string) (string, error) {
	oc, _, err := p.discover()
	if err != nil {
		return "", err
	}
	opts := []oauth2.AuthCodeOption{
		oauth2.S256ChallengeOption(verifier),
		gooidc.Nonce(nonce),
		oauth2.SetAuthURLParam("prompt", "select_account"),
	}
	if len(p.cfg.AllowedDomains) == 1 {
		// a hint only (Google preselects the Workspace domain); the callback checks the domain
		opts = append(opts, oauth2.SetAuthURLParam("hd", strings.TrimSpace(p.cfg.AllowedDomains[0])))
	}
	return oc.AuthCodeURL(state, opts...), nil
}

// idClaims are the claims of the ID token rportd uses. email_verified is a boolean, but some providers send it as
// a string.
type idClaims struct {
	Email         string          `json:"email"`
	EmailVerified json.RawMessage `json:"email_verified"`
	HostedDomain  string          `json:"hd"`
}

func (c idClaims) verified() bool {
	s := strings.Trim(strings.TrimSpace(string(c.EmailVerified)), `"`)
	return strings.EqualFold(s, "true")
}

// Exchange redeems the authorization code with the PKCE verifier and verifies the ID token (signature, issuer,
// audience, expiry and nonce).
func (p *Provider) Exchange(ctx context.Context, code, verifier, nonce string) (*Identity, error) {
	oc, v, err := p.discover()
	if err != nil {
		return nil, err
	}
	// the request's cancellation, the provider's client
	ectx := gooidc.ClientContext(ctx, p.client)
	tok, err := oc.Exchange(ectx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("oidc: code exchange: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return nil, errors.New("oidc: the token response has no id_token")
	}
	idt, err := v.Verify(ectx, raw)
	if err != nil {
		return nil, fmt.Errorf("oidc: ID token: %w", err)
	}
	if idt.Nonce == "" || idt.Nonce != nonce {
		return nil, ErrNonce
	}
	var c idClaims
	if err := idt.Claims(&c); err != nil {
		return nil, fmt.Errorf("oidc: ID token claims: %w", err)
	}
	return &Identity{
		Issuer:        idt.Issuer,
		Subject:       idt.Subject,
		Email:         strings.TrimSpace(c.Email),
		EmailVerified: c.verified(),
		HostedDomain:  c.HostedDomain,
	}, nil
}
