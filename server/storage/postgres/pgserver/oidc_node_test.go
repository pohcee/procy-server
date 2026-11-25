package pgserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/oidc/oidcfake"
	"github.com/openrport/openrport/server/operator"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	pgusers "github.com/openrport/openrport/server/storage/postgres/users"
	"github.com/openrport/openrport/share/tenant"
)

// oidcRedirect is the callback the nodes register; tests send the browser to whichever node they want instead.
const oidcRedirect = "http://127.0.0.1:1/api/v1/oidc/callback"

func oidcOn(t *testing.T, fake *oidcfake.Server) string {
	t.Helper()
	return fmt.Sprintf("[oidc]\n  enabled = true\n  issuer = %q\n  client_id = %q\n  client_secret_file = %q\n  redirect_url = %q\n",
		fake.URL, fake.ClientID, fake.SecretFile(t), oidcRedirect)
}

// oidcSignIn starts a sign-in at h, signs in at the provider and lets the node via handle the callback. It returns
// the fragment of the redirect to the web UI.
func (h *nodeFixture) oidcSignIn(t *testing.T, via *nodeFixture) url.Values {
	t.Helper()
	f, _ := h.oidcSignInBrowser(t, via)
	return f
}

// oidcSignInBrowser is oidcSignIn returning the browser too (it trades the code).
func (h *nodeFixture) oidcSignInBrowser(t *testing.T, via *nodeFixture) (url.Values, *oidcfake.Browser) {
	t.Helper()
	b := oidcfake.NewBrowser(t)
	cb, err := url.Parse(b.Authorize(t, b.Start(t, h.base+"/oidc/login")))
	require.NoError(t, err)
	target, err := url.Parse(via.base)
	require.NoError(t, err)
	cb.Host = target.Host
	_, frag := b.Callback(t, cb.String())
	return frag, b
}

// oidcToken trades a sign-in code at h with the browser b; org "" names none.
func (h *nodeFixture) oidcToken(t *testing.T, b *oidcfake.Browser, code, org string) (int, map[string]any) {
	t.Helper()
	body := map[string]string{"code": code}
	if org != "" {
		body["organization"] = org
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, h.base+"/oidc/token", bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.Client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out := map[string]any{}
	_ = json.Unmarshal(rb, &out)
	out["_raw"] = string(rb)
	code2 := resp.StatusCode
	data, _ := out["data"].(map[string]any)
	if data == nil {
		data = map[string]any{"_raw": out["_raw"]}
	}
	return code2, data
}

// oidcLogin signs user in through OIDC at h and returns the session token.
func (h *nodeFixture) oidcLogin(t *testing.T, fake *oidcfake.Server, sub, email string) string {
	t.Helper()
	fake.SignInAs(oidcfake.User{Subject: sub, Email: email, EmailVerified: true})
	frag, b := h.oidcSignInBrowser(t, h)
	require.NotEmpty(t, frag.Get("code"), frag)
	code, data := h.oidcToken(t, b, frag.Get("code"), "")
	require.Equal(t, http.StatusOK, code, data)
	tok, _ := data["token"].(string)
	require.NotEmpty(t, tok, data)
	return tok
}

func errorCode(out map[string]any) string {
	errs, _ := out["errors"].([]any)
	if len(errs) == 0 {
		return ""
	}
	c, _ := errs[0].(map[string]any)["code"].(string)
	return c
}

func startOIDCNode(t *testing.T) (*nodeFixture, *oidcfake.Server, *pgtest.DB) {
	t.Helper()
	fake := oidcfake.New(t, "procy-client", "procy-secret")
	d := pgtest.New(t)
	seedTenants(t, d)
	h := startNodeOn(t, d, nodeSettings{extra: oidcOn(t, fake)})
	return h, fake, d
}

// Operators sign in through OIDC only; customers keep their passwords until their organization requires OIDC.
func TestNodeOIDCOperatorsAndPolicies(t *testing.T) {
	h, fake, d := startOIDCNode(t)
	ctx := context.Background()
	require.NoError(t, AddUser(ctx, d.App, operator.SystemTenant, "root@example.com", testPassword, nil))

	// an operator with a (legacy) password hash can't use it: not at login, not with basic auth
	code, out := h.do(t, http.MethodPost, "/login", "", map[string]string{"username": "root@example.com", "password": testPassword})
	assert.Equal(t, http.StatusForbidden, code, out["_raw"])
	assert.Equal(t, "oidc_required", errorCode(out))
	code, out = h.do(t, http.MethodPost, "/login", "", map[string]string{"username": "op", "password": testPassword})
	assert.Equal(t, http.StatusForbidden, code, out["_raw"])
	code, out = h.basic(t, http.MethodGet, "/me", "root@example.com", testPassword)
	assert.Equal(t, http.StatusUnauthorized, code, out["_raw"])

	// … but signs in through OIDC
	root := h.oidcLogin(t, fake, "google-root", "root@example.com")
	code, out = h.do(t, http.MethodGet, "/operator/organizations", root, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	code, out = h.do(t, http.MethodGet, "/me", root, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, "oidc", out["data"].(map[string]any)["auth_method"])
	// operators get no password through the API
	code, out = h.do(t, http.MethodPut, "/me", root, map[string]string{"password": "another-long-password-1", "old_password": testPassword})
	assert.Equal(t, http.StatusBadRequest, code, out["_raw"])
	code, out = h.do(t, http.MethodPost, "/users", root, map[string]any{"username": "ops2@example.com", "password": "another-long-password-1",
		"groups": []string{"Administrators"}})
	assert.Equal(t, http.StatusBadRequest, code, out["_raw"])
	code, out = h.do(t, http.MethodPost, "/users", root, map[string]any{"username": "ops2@example.com", "groups": []string{"Administrators"}})
	require.Equal(t, http.StatusCreated, code, "a passwordless operator: %s", out["_raw"])
	h.oidcLogin(t, fake, "google-ops2", "ops2@example.com")

	// a customer keeps the password
	require.NoError(t, AddUser(ctx, d.App, "a", "olga@example.com", testPassword, nil))
	olga := h.login(t, "olga@example.com")

	// an operator requires OIDC for a: olga's password session ends, her password login is refused, OIDC works
	code, out = h.do(t, http.MethodPut, "/operator/organizations/a/sign-in", root, map[string]bool{"require_oidc": true})
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, true, out["data"].(map[string]any)["require_oidc"])
	code, out = h.do(t, http.MethodGet, "/clients", olga, nil)
	assert.Equal(t, http.StatusUnauthorized, code, out["_raw"])
	code, out = h.do(t, http.MethodPost, "/login", "", map[string]string{"username": "olga@example.com", "password": testPassword})
	assert.Equal(t, http.StatusForbidden, code, out["_raw"])
	assert.Equal(t, "oidc_required", errorCode(out))
	olga = h.oidcLogin(t, fake, "google-olga", "olga@example.com")
	code, out = h.do(t, http.MethodGet, "/clients", olga, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	code, out = h.do(t, http.MethodGet, "/organization/sign-in", olga, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, map[string]any{"require_oidc": true, "oidc_enabled": true}, out["data"])

	// b's administrator manages the members' sign-in
	bob := h.login(t, "bob")
	code, out = h.do(t, http.MethodPost, "/users", bob, map[string]any{"username": "pat@example.com", "sign_in": "oidc", "groups": []string{"Viewers"}})
	require.Equal(t, http.StatusCreated, code, "Google only needs no password: %s", out["_raw"])
	code, out = h.do(t, http.MethodPost, "/users", bob, map[string]any{"username": "quinn@example.com", "password": "quinns-long-password-1",
		"sign_in": "password", "groups": []string{"Viewers"}})
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	code, out = h.do(t, http.MethodPost, "/users", bob, map[string]any{"username": "rita@example.com", "groups": []string{"Viewers"}})
	assert.Equal(t, http.StatusBadRequest, code, "a password is still required for both: %s", out["_raw"])
	pat := h.oidcLogin(t, fake, "google-pat", "pat@example.com")
	code, out = h.do(t, http.MethodGet, "/me", pat, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, "oidc", out["data"].(map[string]any)["sign_in"])
	fake.SignInAs(oidcfake.User{Subject: "google-quinn", Email: "quinn@example.com", EmailVerified: true})
	assert.Equal(t, "oidc_not_allowed", h.oidcSignIn(t, h).Get("error"), "quinn is set to password only")
	code, out = h.do(t, http.MethodGet, "/users", bob, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	byName := map[string]map[string]any{}
	for _, u := range out["data"].([]any) {
		byName[u.(map[string]any)["username"].(string)] = u.(map[string]any)
	}
	assert.Equal(t, "oidc", byName["pat@example.com"]["sign_in"])
	assert.Equal(t, true, byName["pat@example.com"]["oidc_linked"])
	assert.Equal(t, "password", byName["quinn@example.com"]["sign_in"])
	assert.Equal(t, true, byName["quinn@example.com"]["oidc_linked"], "a verified sign-in pins even where it is refused")
	assert.Equal(t, false, byName["bob"]["oidc_linked"])
	assert.Equal(t, "both", byName["bob"]["sign_in"])
	code, out = h.do(t, http.MethodPut, "/users/quinn@example.com", bob, map[string]any{"sign_in": "both"})
	require.Equal(t, http.StatusNoContent, code, out["_raw"])
	h.oidcLogin(t, fake, "google-quinn", "quinn@example.com")
	code, out = h.do(t, http.MethodDelete, "/users/quinn@example.com/oidc-identity", bob, nil)
	require.Equal(t, http.StatusNoContent, code, out["_raw"])

	// a member sets their own method: Google only needs a linked Google account
	code, out = h.do(t, http.MethodPut, "/me/sign-in", bob, map[string]string{"method": "oidc"})
	assert.Equal(t, http.StatusConflict, code, out["_raw"])
	code, out = h.do(t, http.MethodPut, "/me/sign-in", pat, map[string]string{"method": "both"})
	assert.Equal(t, http.StatusNoContent, code, out["_raw"])

	// requiring OIDC needs an OIDC session (no lock-out by a password-only administrator)
	code, out = h.do(t, http.MethodPut, "/organization/sign-in", bob, map[string]bool{"require_oidc": true})
	assert.Equal(t, http.StatusConflict, code, out["_raw"])
	code, out = h.do(t, http.MethodPost, "/users", bob, map[string]any{"username": "bea@example.com", "password": "beas-long-password-1",
		"groups": []string{"Administrators"}})
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	bea := h.oidcLogin(t, fake, "google-bea", "bea@example.com")
	code, out = h.do(t, http.MethodPut, "/users/bea@example.com", bea, map[string]any{"sign_in": "oidc"})
	require.Equal(t, http.StatusNoContent, code, "her OIDC session may: %s", out["_raw"])
	code, out = h.do(t, http.MethodPut, "/users/bob", bob, map[string]any{"sign_in": "oidc"})
	assert.Equal(t, http.StatusConflict, code, "bob's own password session may not: %s", out["_raw"])

	// bea requires OIDC for b: bob's password session ends at once
	code, out = h.do(t, http.MethodPut, "/organization/sign-in", bea, map[string]bool{"require_oidc": true})
	require.Equal(t, http.StatusOK, code, out["_raw"])
	code, out = h.do(t, http.MethodGet, "/users", bob, nil)
	assert.Equal(t, http.StatusUnauthorized, code, out["_raw"])
	code, out = h.do(t, http.MethodPut, "/me/sign-in", pat, map[string]string{"method": "password"})
	assert.Equal(t, http.StatusBadRequest, code, "b requires OIDC: %s", out["_raw"])

	// an unknown email is refused and no account is created
	fake.SignInAs(oidcfake.User{Subject: "google-stranger", Email: "stranger@example.com", EmailVerified: true})
	assert.Equal(t, "unknown_account", h.oidcSignIn(t, h).Get("error"))
	var n int
	require.NoError(t, d.Owner.Get(&n, "SELECT count(*) FROM accounts WHERE lower(username) = 'stranger@example.com'"))
	assert.Zero(t, n)

	// sign-ins are in the audit log
	require.NoError(t, d.Owner.Get(&n, "SELECT count(*) FROM auditlog WHERE application = 'auth.oidc' AND action = 'success'"))
	assert.Positive(t, n)
	require.NoError(t, d.Owner.Get(&n, "SELECT count(*) FROM auditlog WHERE application = 'auth.oidc' AND action = 'failed' AND tenant_id = '_system'"))
	assert.Positive(t, n)
}

// A login of several organizations gets the organization picker after OIDC as after a password.
func TestNodeOIDCOrganizationPicker(t *testing.T) {
	h, fake, d := startOIDCNode(t)
	ctx := context.Background()
	require.NoError(t, AddUser(ctx, d.App, "a", "mia@example.com", testPassword, nil))
	store := pgusers.New(d.App)
	ctxB := tenant.WithID(ctx, "b")
	require.NoError(t, store.Invite(ctxB, "mia@example.com", []string{"Viewers"}, "bob"))
	require.NoError(t, store.AcceptInvitation(ctxB, "mia@example.com"))

	fake.SignInAs(oidcfake.User{Subject: "google-mia", Email: "mia@example.com", EmailVerified: true})
	frag, br := h.oidcSignInBrowser(t, h)
	require.NotEmpty(t, frag.Get("code"), frag)
	code, data := h.oidcToken(t, br, frag.Get("code"), "")
	require.Equal(t, http.StatusOK, code, data)
	assert.Nil(t, data["token"])
	assert.Equal(t, []string{"a", "b"}, orgIDs(t, data["organizations"]))

	code, data = h.oidcToken(t, br, frag.Get("code"), "nowhere")
	assert.Equal(t, http.StatusForbidden, code, data)
	code, data = h.oidcToken(t, br, frag.Get("code"), "b")
	require.Equal(t, http.StatusOK, code, data)
	assert.Equal(t, "b", data["organization"])
	assert.Equal(t, []string{"a", "b"}, orgIDs(t, data["organizations"]))
	tokB := data["token"].(string)
	code, out := h.do(t, http.MethodGet, "/me", tokB, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, []any{"Viewers"}, out["data"].(map[string]any)["groups"])
	code, _ = h.oidcToken(t, br, frag.Get("code"), "a")
	assert.Equal(t, http.StatusUnauthorized, code, "the code was used")

	// switching keeps the sign-in method: an OIDC session may enter an organization that requires OIDC, a
	// password session may not
	_, err := d.Owner.Exec("UPDATE tenants SET require_oidc = true WHERE id = 'a'")
	require.NoError(t, err)
	code, out = h.do(t, http.MethodPost, "/me/organization", tokB, map[string]string{"organization": "a"})
	require.Equal(t, http.StatusOK, code, out["_raw"])
	tokA := out["data"].(map[string]any)["token"].(string)
	code, out = h.do(t, http.MethodGet, "/me", tokA, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, "oidc", out["data"].(map[string]any)["auth_method"])
	code, out = h.do(t, http.MethodPost, "/login?organization=b", "", map[string]string{"username": "mia@example.com", "password": testPassword})
	require.Equal(t, http.StatusOK, code, out["_raw"])
	pwB := out["data"].(map[string]any)["token"].(string)
	code, out = h.do(t, http.MethodPost, "/me/organization", pwB, map[string]string{"organization": "a"})
	assert.Equal(t, http.StatusForbidden, code, out["_raw"])
	assert.Equal(t, "oidc_required", errorCode(out))

	// an organization that refuses OIDC for mia drops out of the list: one left, a token at once
	_, err = d.Owner.Exec("UPDATE users SET sign_in = 'password' WHERE tenant_id = 'b' AND username = 'mia@example.com'")
	require.NoError(t, err)
	frag, br = h.oidcSignInBrowser(t, h)
	code, data = h.oidcToken(t, br, frag.Get("code"), "")
	require.Equal(t, http.StatusOK, code, data)
	assert.NotEmpty(t, data["token"])
	assert.NotContains(t, data, "organizations")
}

// Any node of a cluster takes any step: the sign-in starts at one node, the provider sends the browser back to the
// other, and the code is traded at the first.
func TestNodeOIDCCallbackOnAnotherNode(t *testing.T) {
	fake := oidcfake.New(t, "procy-client", "procy-secret")
	d := pgtest.New(t)
	seedTenants(t, d)
	require.NoError(t, AddUser(context.Background(), d.App, "a", "nina@example.com", testPassword, nil))
	extra := oidcOn(t, fake)
	one := startNodeOn(t, d, nodeSettings{nodeID: "node-a", extra: extra})
	two := startNodeOn(t, d, nodeSettings{nodeID: "node-b", extra: extra})

	fake.SignInAs(oidcfake.User{Subject: "google-nina", Email: "nina@example.com", EmailVerified: true})
	frag, br := one.oidcSignInBrowser(t, two)
	require.NotEmpty(t, frag.Get("code"), frag)
	code, data := one.oidcToken(t, br, frag.Get("code"), "")
	require.Equal(t, http.StatusOK, code, data)
	tok := data["token"].(string)
	code, out := two.do(t, http.MethodGet, "/clients", tok, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, []string{"a-web"}, clientNames(t, out))

	// a state is taken once, whichever node sees it again
	b := oidcfake.NewBrowser(t)
	cb, err := url.Parse(b.Authorize(t, b.Start(t, one.base+"/oidc/login")))
	require.NoError(t, err)
	for i, node := range []*nodeFixture{two, one} {
		target, err := url.Parse(node.base)
		require.NoError(t, err)
		cb.Host = target.Host
		_, f := b.Callback(t, cb.String())
		if i == 0 {
			assert.NotEmpty(t, f.Get("code"), f)
		} else {
			assert.Equal(t, "invalid_state", f.Get("error"), "replayed on the other node")
		}
	}
}
