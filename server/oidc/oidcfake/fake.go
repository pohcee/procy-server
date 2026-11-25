// Package oidcfake is a fake OpenID Connect provider for tests: an httptest server with discovery, a JWKS, an
// authorization endpoint that signs in whoever SignInAs names (no consent page) and a token endpoint that checks the
// client secret and the PKCE verifier and issues ID tokens signed with a test key. No network.
package oidcfake

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"
)

const keyID = "test-key"

// User is who signs in at the fake provider next.
type User struct {
	Subject       string
	Email         string
	EmailVerified bool
	HostedDomain  string
}

type grant struct {
	user          User
	nonce         string
	challenge     string
	redirectURI   string
	clientID      string
	challengeMeth string
}

// Server is the fake provider.
type Server struct {
	*httptest.Server
	ClientID, ClientSecret string

	key *rsa.PrivateKey
	mu  sync.Mutex
	// next signs in at the authorization endpoint
	next   User
	grants map[string]grant
	// NonceOverride, when set, is put in ID tokens instead of the sign-in's nonce.
	NonceOverride string
	// Deny makes the authorization endpoint answer error=access_denied (the user canceled).
	Deny bool
}

// New starts a fake provider for the client clientID with secret.
func New(t testing.TB, clientID, secret string) *Server {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	s := &Server{ClientID: clientID, ClientSecret: secret, key: key, grants: map[string]grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", s.discovery)
	mux.HandleFunc("/jwks", s.jwks)
	mux.HandleFunc("/authorize", s.authorize)
	mux.HandleFunc("/token", s.token)
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// SignInAs sets who signs in next.
func (s *Server) SignInAs(u User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next = u
}

// SecretFile writes the client secret to a file for [oidc] client_secret_file.
func (s *Server) SecretFile(t testing.TB) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "oidc-secret")
	require.NoError(t, os.WriteFile(p, []byte(s.ClientSecret+"\n"), 0600))
	return p
}

func (s *Server) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                s.URL,
		"authorization_endpoint":                s.URL + "/authorize",
		"token_endpoint":                        s.URL + "/token",
		"jwks_uri":                              s.URL + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (s *Server) jwks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &s.key.PublicKey, KeyID: keyID, Algorithm: "RS256", Use: "sig"}}})
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || q.Get("response_type") != "code" || q.Get("client_id") != s.ClientID {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	back := redirect.Query()
	back.Set("state", q.Get("state"))
	s.mu.Lock()
	deny := s.Deny
	if !deny {
		code := randomString()
		s.grants[code] = grant{user: s.next, nonce: q.Get("nonce"), challenge: q.Get("code_challenge"),
			challengeMeth: q.Get("code_challenge_method"), redirectURI: redirect.String(), clientID: q.Get("client_id")}
		back.Set("code", code)
	}
	s.mu.Unlock()
	if deny {
		back.Set("error", "access_denied")
	}
	redirect.RawQuery = back.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound) //nolint:gosec // G710: a test provider redirects where the test says

}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "authorization_code" {
		tokenError(w, "invalid_request")
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != s.ClientID || secret != s.ClientSecret {
		tokenError(w, "invalid_client")
		return
	}
	s.mu.Lock()
	g, found := s.grants[r.PostForm.Get("code")]
	delete(s.grants, r.PostForm.Get("code"))
	nonce := g.nonce
	if s.NonceOverride != "" {
		nonce = s.NonceOverride
	}
	s.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !found || g.redirectURI != r.PostForm.Get("redirect_uri") || g.challengeMeth != "S256" ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		tokenError(w, "invalid_grant")
		return
	}
	now := time.Now()
	claims := map[string]any{
		"iss": s.URL, "aud": s.ClientID, "sub": g.user.Subject, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"nonce": nonce, "email": g.user.Email, "email_verified": g.user.EmailVerified,
	}
	if g.user.HostedDomain != "" {
		claims["hd"] = g.user.HostedDomain
	}
	writeJSON(w, map[string]any{"access_token": randomString(), "token_type": "Bearer", "expires_in": 3600,
		"id_token": s.Sign(claims)})
}

// Sign returns claims as an ID token signed with the provider's key.
func (s *Server) Sign(claims map[string]any) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: s.key, KeyID: keyID}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		panic(err)
	}
	b, err := json.Marshal(claims)
	if err != nil {
		panic(err)
	}
	obj, err := signer.Sign(b)
	if err != nil {
		panic(err)
	}
	out, err := obj.CompactSerialize()
	if err != nil {
		panic(err)
	}
	return out
}

func tokenError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func randomString() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
