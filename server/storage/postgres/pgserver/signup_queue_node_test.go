package pgserver

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/oidc/oidcfake"
	"github.com/openrport/openrport/server/operator"
)

const signupNotice = "Preview: we're just starting out; features and plans may change."

// queueRows returns the operators' signup list by email.
func queueRows(t *testing.T, h *nodeFixture, tok, status string) map[string]map[string]any {
	t.Helper()
	code, out := h.do(t, http.MethodGet, "/operator/signups?status="+status, tok, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	rows := map[string]map[string]any{}
	for _, r := range out["data"].([]any) {
		m := r.(map[string]any)
		rows[m["email"].(string)] = m
	}
	return rows
}

// requestAndVerify signs up email for org through the emailed link and returns the signup's id.
func requestAndVerify(t *testing.T, h *nodeFixture, sink *mailSink, op, email, org string) string {
	t.Helper()
	code, out := h.do(t, http.MethodPost, "/signup", "", signupBody(email, org))
	require.Equal(t, http.StatusAccepted, code, out["_raw"])
	code, out = h.do(t, http.MethodPost, "/signup/verify", "", map[string]string{"token": sink.last(t, email).token(t), "password": signupPassword})
	require.Equal(t, http.StatusAccepted, code, out["_raw"])
	return queueRows(t, h, op, "awaiting_approval")[email]["id"].(string)
}

// A verified signup waits in the queue with what the operator needs to decide, the operators are told by email,
// and the emails carry [signup] email_notice.
func TestSignupQueueNotifyAndNotice(t *testing.T) {
	h, sink := startSignupNode(t, "require_approval = true\n  max_per_ip_per_day = -1\n  notify_email = \"Ops@Example.com\"\n  email_notice = \""+signupNotice+"\"\n")
	op := h.login(t, "op")

	id := requestAndVerify(t, h, sink, op, "fay@acme.example", "Acme")
	n := sink.last(t, "ops@example.com")
	assert.Equal(t, `New signup waiting: "Acme"`, n.subject)
	for _, want := range []string{"fay@acme.example", "acme.example", "Mail server:  yes", "the emailed link", "https://ui.example/operator/signups"} {
		assert.Contains(t, n.body, want)
	}
	assert.NotContains(t, n.body, signupNotice, "the operators' email has no notice")
	for _, m := range sink.all() {
		if m.to == "fay@acme.example" {
			assert.Contains(t, m.body, signupNotice, m.subject)
		}
	}

	row := queueRows(t, h, op, "awaiting_approval")["fay@acme.example"]
	assert.Equal(t, "email", row["method"])
	assert.Equal(t, true, row["mx_found"])
	assert.Equal(t, false, row["free_mail"])
	assert.EqualValues(t, 0, row["domain_customers"])
	assert.NotEmpty(t, row["verified_at"])

	// approve: the organization exists, the email says how to sign in
	code, out := h.do(t, http.MethodPost, "/operator/signups/"+id+"/approve", op, nil)
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	ready := sink.last(t, "fay@acme.example")
	assert.Contains(t, ready.subject, "is ready")
	assert.Contains(t, ready.body, "https://ui.example/auth")
	assert.Contains(t, ready.body, "the password you chose")
	assert.Contains(t, ready.body, signupNotice)

	// the next signup of the domain shows it is a customer already; a rejection is mailed with its reason
	id2 := requestAndVerify(t, h, sink, op, "gil@acme.example", "Acme Two")
	row = queueRows(t, h, op, "awaiting_approval")["gil@acme.example"]
	assert.EqualValues(t, 1, row["domain_customers"])
	code, out = h.do(t, http.MethodPost, "/operator/signups/"+id2+"/deny", op, map[string]string{"reason": "one organization per company for now"})
	require.Equal(t, http.StatusNoContent, code, out["_raw"])
	denied := sink.last(t, "gil@acme.example")
	assert.Contains(t, denied.body, "not approved")
	assert.Contains(t, denied.body, "one organization per company for now")

	// … and the next one of that email and domain shows the earlier rejection
	requestAndVerify(t, h, sink, op, "gil@acme.example", "Acme Three")
	row = queueRows(t, h, op, "awaiting_approval")["gil@acme.example"]
	assert.EqualValues(t, 1, row["email_denied_before"])
	assert.EqualValues(t, 1, row["domain_denied_before"])

	// free mail: no domain checks
	requestAndVerify(t, h, sink, op, "hal@gmail.com", "Hal's")
	row = queueRows(t, h, op, "awaiting_approval")["hal@gmail.com"]
	assert.Equal(t, true, row["free_mail"])
	assert.EqualValues(t, 0, row["domain_customers"])
	assert.Contains(t, sink.last(t, "ops@example.com").body, "gmail.com (free mail)")
}

// The domain without a mail server is shown as such (check_mx off: not refused).
func TestSignupQueueNoMX(t *testing.T) {
	signupOverrides.lookupMX = func(_ context.Context, d string) error {
		if d == "no-mx.example" {
			return assert.AnError
		}
		return nil
	}
	h, sink := startSignupNode(t, "require_approval = true\n")
	op := h.login(t, "op")
	requestAndVerify(t, h, sink, op, "ian@no-mx.example", "Ian")
	assert.Equal(t, false, queueRows(t, h, op, "awaiting_approval")["ian@no-mx.example"]["mx_found"])
}

// The plan catalog is public for the home page, without provider ids; the signup page learns about Google.
func TestPublicPlansAndSignupConfig(t *testing.T) {
	h, _ := startSignupNode(t, "")
	code, out := h.do(t, http.MethodGet, "/plans", "", nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	plans := out["data"].([]any)
	require.NotEmpty(t, plans)
	assert.NotContains(t, out["_raw"], "provider", "no provider price ids")
	code, out = h.do(t, http.MethodGet, "/signup", "", nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, false, data(t, out)["google_signup"], "no [oidc]")
}

// startGoogleSignupNode runs a node with [signup] (settings added) and [oidc] on a fake provider, with an operator
// root@example.com who signs in with Google.
func startGoogleSignupNode(t *testing.T, settings string) (*nodeFixture, *mailSink, *oidcfake.Server, string) {
	t.Helper()
	fake := oidcfake.New(t, "procy-client", "procy-secret")
	h, sink := startSignupNode(t, settings+oidcOn(t, fake))
	require.NoError(t, AddUser(context.Background(), h.db.App, operator.SystemTenant, "root@example.com", testPassword, nil))
	op := h.oidcLogin(t, fake, "google-root", "root@example.com")
	return h, sink, fake, op
}

// googleSignup runs a signup with Google for org at h as the provider's current user and returns the web UI page
// it ends on and the fragment.
func googleSignup(t *testing.T, h *nodeFixture, org string) (string, url.Values) {
	t.Helper()
	b := oidcfake.NewBrowser(t)
	cb, err := url.Parse(b.Authorize(t, b.Start(t, h.base+"/oidc/signup?organization="+url.QueryEscape(org))))
	require.NoError(t, err)
	target, err := url.Parse(h.base)
	require.NoError(t, err)
	cb.Host = target.Host
	return b.Callback(t, cb.String())
}

// With Google the verified email replaces the link: the signup waits for approval at once, nothing exists before
// it, and the approved account signs in with Google.
func TestSignupWithGoogle(t *testing.T) {
	h, sink, fake, op := startGoogleSignupNode(t, "require_approval = true\n  notify_email = \"ops@example.com\"\n")

	code, out := h.do(t, http.MethodGet, "/signup", "", nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	cfg := data(t, out)
	assert.Equal(t, true, cfg["google_signup"])
	assert.Equal(t, "/api/v1/oidc/signup", cfg["google_signup_url"])

	fake.SignInAs(oidcfake.User{Subject: "google-kim", Email: "Kim@Gmail.com", EmailVerified: true})
	ui, frag := googleSignup(t, h, "Kim's Lab")
	assert.True(t, strings.HasSuffix(ui, "/auth/signup/google"), ui)
	require.Equal(t, "awaiting_approval", frag.Get("status"), frag)
	assert.Equal(t, "Kim's Lab", frag.Get("organization_name"))
	assert.Equal(t, "kim@gmail.com", frag.Get("email"))
	assert.Contains(t, sink.last(t, "kim@gmail.com").subject, "waiting for approval")
	assert.Contains(t, sink.last(t, "ops@example.com").body, "Google sign-in")

	// nothing exists yet: Google sign-in finds no account
	frag, _ = h.oidcSignInBrowser(t, h)
	assert.Equal(t, "unknown_account", frag.Get("error"), frag)

	row := queueRows(t, h, op, "awaiting_approval")["kim@gmail.com"]
	require.NotNil(t, row)
	assert.Equal(t, "google", row["method"])
	assert.Equal(t, true, row["free_mail"])
	code, out = h.do(t, http.MethodPost, "/operator/signups/"+row["id"].(string)+"/approve", op, nil)
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	org := data(t, out)["organization"].(string)
	assert.Contains(t, sink.last(t, "kim@gmail.com").body, "Sign in with Google")

	kim := h.oidcLogin(t, fake, "google-kim", "kim@gmail.com")
	code, out = h.do(t, http.MethodGet, "/me", kim, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	code, _ = h.loginPw(t, "kim@gmail.com", signupPassword)
	assert.Equal(t, http.StatusUnauthorized, code, "no password")
	_ = org

	// a second signup of the same (now proven) address: sign in instead
	_, frag = googleSignup(t, h, "Another")
	assert.Equal(t, "account_exists", frag.Get("error"), frag)

	// (an address Google doesn't manage is refused like at sign-in: oidc.LinkableEmail, Google's issuer only)
	fake.SignInAs(oidcfake.User{Subject: "google-mo", Email: "mo@gmail.com", EmailVerified: false})
	_, frag = googleSignup(t, h, "Mo")
	assert.Equal(t, "email_unverified", frag.Get("error"), frag)

	// a bad name never reaches the provider
	b := oidcfake.NewBrowser(t)
	loc, err := url.Parse(b.Start(t, h.base+"/oidc/signup?organization="))
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(loc.Path, "/auth/signup/google"), loc.String())
	f, _ := url.ParseQuery(loc.Fragment)
	assert.Equal(t, "invalid", f.Get("error"))
}

// Without require_approval a Google signup creates the organization at once.
func TestSignupWithGoogleNoApproval(t *testing.T) {
	h, _, fake, _ := startGoogleSignupNode(t, "")
	fake.SignInAs(oidcfake.User{Subject: "google-ned", Email: "ned@corp.example", EmailVerified: true, HostedDomain: "corp.example"})
	_, frag := googleSignup(t, h, "Corp")
	require.Equal(t, "completed", frag.Get("status"), frag)
	assert.NotEmpty(t, frag.Get("organization"))
	ned := h.oidcLogin(t, fake, "google-ned", "ned@corp.example")
	code, out := h.do(t, http.MethodGet, "/me", ned, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
}
