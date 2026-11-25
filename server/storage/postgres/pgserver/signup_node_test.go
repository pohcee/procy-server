package pgserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/operator"
	"github.com/openrport/openrport/server/signup"
	pgsignup "github.com/openrport/openrport/server/storage/postgres/signup"
	"github.com/openrport/openrport/share/tenant"
)

// mailSink records the signup emails.
type mailSink struct {
	mu   sync.Mutex
	sent []sentMail
}

type sentMail struct{ to, subject, body string }

func (m *mailSink) Send(_ context.Context, to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentMail{to, subject, body})
	return nil
}

func (m *mailSink) all() []sentMail {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]sentMail(nil), m.sent...)
}

func (m *mailSink) last(t *testing.T, to string) sentMail {
	t.Helper()
	all := m.all()
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].to == to {
			return all[i]
		}
	}
	t.Fatalf("no email to %s in %v", to, all)
	return sentMail{}
}

var tokenInLink = regexp.MustCompile(`\?token=([A-Za-z0-9_%-]+)`)

func (m sentMail) token(t *testing.T) string {
	t.Helper()
	g := tokenInLink.FindStringSubmatch(m.body)
	require.NotNil(t, g, "no link in %q", m.body)
	tok, err := url.QueryUnescape(g[1])
	require.NoError(t, err)
	return tok
}

// fakeCaptcha accepts the response "solved".
type fakeCaptcha struct{}

func (fakeCaptcha) Verify(_ context.Context, response, _ string) error {
	if response != "solved" {
		return signup.ErrCaptcha
	}
	return nil
}

const signupPassword = "a much longer passphrase 1984"

// startSignupNode runs a node with [signup] on (settings are added to the section) and the fake mailer and
// captcha.
func startSignupNode(t *testing.T, settings string) (*nodeFixture, *mailSink) {
	t.Helper()
	sink := &mailSink{}
	signupOverrides.mailer, signupOverrides.captcha = sink, fakeCaptcha{}
	if signupOverrides.lookupMX == nil {
		signupOverrides.lookupMX = func(context.Context, string) error { return nil }
	}
	t.Cleanup(func() {
		signupOverrides.mailer, signupOverrides.captcha, signupOverrides.lookupMX, signupOverrides.plans = nil, nil, nil, nil
	})
	h := startNodeWith(t, nodeSettings{extra: `
[signup]
  enabled = true
  public_url = "https://ui.example"
  mail = "log"
  trial_plan = "team"
  captcha = "turnstile"
  captcha_secret_file = "` + secretFile(t) + `"
` + settings})
	return h, sink
}

func secretFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "captcha")
	require.NoError(t, os.WriteFile(p, []byte("secret\n"), 0600))
	return p
}

func signupBody(email, org string) map[string]string {
	return map[string]string{"email": email, "organization": org, "captcha": "solved"}
}

func (h *nodeFixture) loginPw(t *testing.T, user, password string) (int, string) {
	t.Helper()
	code, out := h.do(t, http.MethodPost, "/login", "", map[string]string{"username": user, "password": password})
	if code != http.StatusOK {
		return code, ""
	}
	return code, data(t, out)["token"].(string)
}

func TestSignupFullFlow(t *testing.T) {
	h, sink := startSignupNode(t, "")
	op := h.login(t, "op")
	assert.Equal(t, true, capabilities(t, h, op)["signup"])

	code, out := h.do(t, http.MethodGet, "/signup", "", nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, "turnstile", data(t, out)["captcha"])
	assert.EqualValues(t, 14, data(t, out)["trial_days"])

	// refused before any email: bad captcha, bad email, control characters in the name, a password (chosen at verify)
	body := signupBody("Dana@Example.COM", "Dana's Widgets, Inc.")
	body["captcha"] = "robot"
	code, out = h.do(t, http.MethodPost, "/signup", "", body)
	assert.Equal(t, http.StatusBadRequest, code, out["_raw"])
	code, _ = h.do(t, http.MethodPost, "/signup", "", signupBody("not-an-email", "x"))
	assert.Equal(t, http.StatusBadRequest, code)
	code, _ = h.do(t, http.MethodPost, "/signup", "", signupBody("dana@example.com", "Evil\r\nBcc: x@y.z"))
	assert.Equal(t, http.StatusBadRequest, code)
	withPassword := signupBody("dana@example.com", "x")
	withPassword["password"] = signupPassword
	code, _ = h.do(t, http.MethodPost, "/signup", "", withPassword)
	assert.Equal(t, http.StatusBadRequest, code, "unknown field")
	assert.Empty(t, sink.all())

	code, out = h.do(t, http.MethodPost, "/signup", "", signupBody("Dana@Example.COM", "Dana's Widgets, Inc."))
	require.Equal(t, http.StatusAccepted, code, out["_raw"])
	m := sink.last(t, "dana@example.com")
	assert.Contains(t, m.body, "https://ui.example/auth/signup/verify?token=")
	tok := m.token(t)

	// the token is stored hashed only
	var stored []string
	require.NoError(t, h.db.Owner.Select(&stored, "SELECT coalesce(token_hash, '') FROM signups"))
	require.Len(t, stored, 1)
	assert.NotContains(t, stored[0], tok)

	// a weak password is refused and the link still works
	code, out = h.do(t, http.MethodPost, "/signup/verify", "", map[string]string{"token": tok, "password": "short"})
	assert.Equal(t, http.StatusBadRequest, code, out["_raw"])
	code, out = h.do(t, http.MethodPost, "/signup/verify", "", map[string]string{"token": tok, "password": signupPassword})
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	res := data(t, out)
	assert.Equal(t, "completed", res["status"])
	assert.Equal(t, false, res["checkout_required"])
	assert.Equal(t, "team", res["plan"].(map[string]any)["plan"])
	assert.Equal(t, "trial", res["plan"].(map[string]any)["billing_status"])
	org := res["organization"].(string)
	assert.Regexp(t, `^dana-s-widgets-inc-[a-z0-9]{6}$`, org)

	// single use
	code, out = h.do(t, http.MethodPost, "/signup/verify", "", map[string]string{"token": tok, "password": signupPassword})
	assert.Equal(t, http.StatusBadRequest, code, out["_raw"])

	// the new administrator logs in with the password they chose, in the new organization
	code, dana := h.loginPw(t, "dana@example.com", signupPassword)
	require.Equal(t, http.StatusOK, code)
	code, out = h.do(t, http.MethodGet, "/me", dana, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, []any{"Administrators"}, data(t, out)["groups"])
	code, out = h.do(t, http.MethodGet, "/operator/organizations/"+org, op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.EqualValues(t, 25, data(t, out)["device_limit"], "constrained until a card is on file")
	assert.Equal(t, "Dana's Widgets, Inc.", data(t, out)["name"])

	// the email is a user now: the same answer, a notice instead of a link
	code, out = h.do(t, http.MethodPost, "/signup", "", signupBody("dana@example.com", "Another"))
	require.Equal(t, http.StatusAccepted, code, out["_raw"])
	m = sink.last(t, "dana@example.com")
	assert.Equal(t, "You already have an account", m.subject)
	assert.NotContains(t, m.body, "token=")

	// operators list the signups
	code, out = h.do(t, http.MethodGet, "/operator/signups", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	list := out["data"].([]any)
	require.Len(t, list, 2)
	assert.Equal(t, "existing", list[0].(map[string]any)["status"])
	assert.Equal(t, "completed", list[1].(map[string]any)["status"])
	assert.Equal(t, org, list[1].(map[string]any)["organization_id"])
	assert.NotContains(t, out["_raw"], "password")
	code, _ = h.do(t, http.MethodGet, "/operator/signups", dana, nil)
	assert.Equal(t, http.StatusNotFound, code, "not for organization admins")

	var hash string
	require.NoError(t, h.db.Owner.Get(&hash, "SELECT password_hash FROM signups WHERE status = 'completed'"))
	assert.Empty(t, hash, "the password hash is cleared once used")

	var actions []string
	require.NoError(t, h.db.Owner.Select(&actions, `SELECT application || ' ' || action || ' ' || username || ' ' || remote_ip FROM auditlog
		WHERE tenant_id = '_system' ORDER BY id`))
	assert.Contains(t, actions, "signup request signup:dana@example.com 127.0.0.1")
	assert.Contains(t, actions, "operator.organization create signup:dana@example.com 127.0.0.1")
	assert.Contains(t, actions, "operator.organization.admin create signup:dana@example.com 127.0.0.1")
	assert.Contains(t, actions, "signup create signup:dana@example.com 127.0.0.1")
}

func TestSignupTokenExpiry(t *testing.T) {
	h, sink := startSignupNode(t, "")
	code, out := h.do(t, http.MethodPost, "/signup", "", signupBody("erin@example.com", "Erin"))
	require.Equal(t, http.StatusAccepted, code, out["_raw"])
	tok := sink.last(t, "erin@example.com").token(t)
	_, err := h.db.Owner.Exec("UPDATE signups SET expires_at = now() - interval '1 second'")
	require.NoError(t, err)
	code, out = h.do(t, http.MethodPost, "/signup/verify", "", map[string]string{"token": tok, "password": signupPassword})
	assert.Equal(t, http.StatusBadRequest, code, out["_raw"])
	code, out = h.do(t, http.MethodPost, "/signup/verify", "", map[string]string{"token": "made-up", "password": signupPassword})
	assert.Equal(t, http.StatusBadRequest, code, out["_raw"])
	code, _ = h.loginPw(t, "erin@example.com", signupPassword)
	assert.Equal(t, http.StatusUnauthorized, code)
}

func TestSignupRateLimitPerIP(t *testing.T) {
	h, _ := startSignupNode(t, "max_per_ip_per_day = 2\n")
	for i, want := range []int{http.StatusAccepted, http.StatusAccepted, http.StatusTooManyRequests} {
		code, out := h.do(t, http.MethodPost, "/signup", "", signupBody(fmt.Sprintf("u%d@d%d.example", i, i), "Org"))
		assert.Equal(t, want, code, "%d: %s", i, out["_raw"])
	}
}

func TestSignupRateLimitPerDomainAndEmail(t *testing.T) {
	h, _ := startSignupNode(t, "max_per_ip_per_day = -1\nmax_per_domain_per_day = 2\nmax_per_email_per_day = 1\n")
	code, _ := h.do(t, http.MethodPost, "/signup", "", signupBody("a@same.example", "Org"))
	assert.Equal(t, http.StatusAccepted, code)
	code, _ = h.do(t, http.MethodPost, "/signup", "", signupBody("A@same.example", "Org"))
	assert.Equal(t, http.StatusTooManyRequests, code, "per email, case-insensitive")
	code, _ = h.do(t, http.MethodPost, "/signup", "", signupBody("b@same.example", "Org"))
	assert.Equal(t, http.StatusAccepted, code, "a refused request counts for nothing")
	code, _ = h.do(t, http.MethodPost, "/signup", "", signupBody("c@same.example", "Org"))
	assert.Equal(t, http.StatusTooManyRequests, code, "per domain")
	code, _ = h.do(t, http.MethodPost, "/signup", "", signupBody("c@other.example", "Org"))
	assert.Equal(t, http.StatusAccepted, code)
}

func TestSignupBlockedDomains(t *testing.T) {
	list := filepath.Join(t.TempDir(), "blocked.txt")
	require.NoError(t, os.WriteFile(list, []byte("# disposable\nmailinator.com\n"), 0600))
	h, sink := startSignupNode(t, fmt.Sprintf("blocked_domains_file = %q\n", list))
	for _, e := range []string{"x@mailinator.com", "x@eu.mailinator.com", "x@MAILINATOR.com"} {
		code, out := h.do(t, http.MethodPost, "/signup", "", signupBody(e, "Org"))
		assert.Equal(t, http.StatusBadRequest, code, "%s: %s", e, out["_raw"])
	}
	assert.Empty(t, sink.all())
	code, _ := h.do(t, http.MethodPost, "/signup", "", signupBody("x@notmailinator.com", "Org"))
	assert.Equal(t, http.StatusAccepted, code)
}

func TestSignupMXCheck(t *testing.T) {
	signupOverrides.lookupMX = func(_ context.Context, domain string) error {
		if domain == "no-mx.example" {
			return errors.New("no mail server")
		}
		return nil
	}
	h, _ := startSignupNode(t, "check_mx = true\n")
	code, _ := h.do(t, http.MethodPost, "/signup", "", signupBody("x@has-mx.example", "Org"))
	assert.Equal(t, http.StatusAccepted, code)
	code, _ = h.do(t, http.MethodPost, "/signup", "", signupBody("x@no-mx.example", "Org"))
	assert.Equal(t, http.StatusBadRequest, code)
}

func TestSignupRequireApproval(t *testing.T) {
	h, sink := startSignupNode(t, "require_approval = true\n")
	op := h.login(t, "op")
	for _, e := range []string{"fay@example.com", "gus@example.com"} {
		code, out := h.do(t, http.MethodPost, "/signup", "", signupBody(e, "Org of "+e))
		require.Equal(t, http.StatusAccepted, code, out["_raw"])
		code, out = h.do(t, http.MethodPost, "/signup/verify", "", map[string]string{"token": sink.last(t, e).token(t), "password": signupPassword})
		require.Equal(t, http.StatusAccepted, code, out["_raw"])
		assert.Equal(t, "awaiting_approval", data(t, out)["status"])
	}
	code, _ := h.loginPw(t, "fay@example.com", signupPassword)
	assert.Equal(t, http.StatusUnauthorized, code, "nothing is created before the approval")

	code, out := h.do(t, http.MethodGet, "/operator/signups?status=awaiting_approval", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	ids := map[string]string{}
	for _, s := range out["data"].([]any) {
		m := s.(map[string]any)
		ids[m["email"].(string)] = m["id"].(string)
	}
	require.Len(t, ids, 2)

	code, out = h.do(t, http.MethodPost, "/operator/signups/"+ids["fay@example.com"]+"/approve", op, nil)
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	assert.Contains(t, sink.last(t, "fay@example.com").subject, "is ready")
	code, _ = h.loginPw(t, "fay@example.com", signupPassword)
	assert.Equal(t, http.StatusOK, code)
	code, _ = h.do(t, http.MethodPost, "/operator/signups/"+ids["fay@example.com"]+"/approve", op, nil)
	assert.Equal(t, http.StatusConflict, code, "approved already")

	code, out = h.do(t, http.MethodPost, "/operator/signups/"+ids["gus@example.com"]+"/deny", op, map[string]string{"reason": "spam"})
	require.Equal(t, http.StatusNoContent, code, out["_raw"])
	code, _ = h.loginPw(t, "gus@example.com", signupPassword)
	assert.Equal(t, http.StatusUnauthorized, code)
	code, _ = h.do(t, http.MethodPost, "/operator/signups/nope/approve", op, nil)
	assert.Equal(t, http.StatusNotFound, code)
}

// signUp creates an organization through the signup flow and returns its id and the admin's token.
func signUp(t *testing.T, h *nodeFixture, sink *mailSink, email string) (string, string) {
	t.Helper()
	code, out := h.do(t, http.MethodPost, "/signup", "", signupBody(email, "Org"))
	require.Equal(t, http.StatusAccepted, code, out["_raw"])
	code, out = h.do(t, http.MethodPost, "/signup/verify", "", map[string]string{"token": sink.last(t, email).token(t), "password": signupPassword})
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	org := data(t, out)["organization"].(string)
	code, tok := h.loginPw(t, email, signupPassword)
	require.Equal(t, http.StatusOK, code)
	return org, tok
}

func TestOrganizationExport(t *testing.T) {
	h, sink := startSignupNode(t, "")
	// a stored tunnel with a password in its options, and a non-administrator
	_, err := h.db.Owner.Exec(`INSERT INTO stored_tunnels (tenant_id, id, client_id, created_at, name, further_options)
		VALUES ('a', 'st1', 'web01', now(), 'rdp', '{"auth_user": "u", "auth_password": "hunter2"}')`)
	require.NoError(t, err)
	require.NoError(t, AddUser(context.Background(), h.db.App, "a", "carl", testPassword, []string{"Viewers"}))
	carl := h.login(t, "carl")
	code, _ := h.do(t, http.MethodGet, "/organization/export", carl, nil)
	assert.Equal(t, http.StatusForbidden, code, "administrators only")
	code, _ = h.do(t, http.MethodPost, "/organization/deletion", carl, map[string]string{"confirm": "a"})
	assert.Equal(t, http.StatusForbidden, code, "administrators only")

	alice := h.login(t, "alice")
	code, out := h.do(t, http.MethodGet, "/organization/export", alice, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.NotContains(t, out["_raw"], "hunter2")
	assert.Contains(t, out["_raw"], "auth_user")
	e := data(t, out)
	assert.Equal(t, signup.ExportFormat, e["format"])
	assert.Equal(t, "a", e["organization"].(map[string]any)["id"])
	users := e["users"].([]any)
	require.Len(t, users, 2)
	assert.Equal(t, "alice", users[0].(map[string]any)["username"])
	clients := e["clients"].([]any)
	require.Len(t, clients, 1)
	assert.Equal(t, "web01", clients[0].(map[string]any)["id"])
	assert.NotEmpty(t, e["library"], "the built-in entries")
	raw := strings.ToLower(out["_raw"].(string))
	assert.NotContains(t, raw, "password")
	assert.NotContains(t, raw, "totp")
	assert.NotContains(t, raw, "b-web", "only its own organization")

	_, owner := signUp(t, h, sink, "owner@example.com")
	code, out = h.do(t, http.MethodGet, "/organization/export", owner, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, "owner@example.com", data(t, out)["users"].([]any)[0].(map[string]any)["username"])
	code, _ = h.do(t, http.MethodGet, "/organization/export", h.login(t, "op"), nil)
	assert.Equal(t, http.StatusForbidden, code, "not in the system organization")

	op := h.login(t, "op")
	code, out = h.do(t, http.MethodGet, "/operator/organizations/b/export", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Contains(t, out["_raw"], "b-web")
	code, _ = h.do(t, http.MethodGet, "/operator/organizations/_system/export", op, nil)
	assert.Equal(t, http.StatusNotFound, code)
}

func TestOrganizationDeletionHoldCancelAndExpedite(t *testing.T) {
	h, sink := startSignupNode(t, "")
	op := h.login(t, "op")
	org, admin := signUp(t, h, sink, "hal@example.com")

	code, out := h.do(t, http.MethodPost, "/organization/deletion", admin, map[string]string{"confirm": "wrong"})
	assert.Equal(t, http.StatusBadRequest, code, out["_raw"])
	code, out = h.do(t, http.MethodPost, "/organization/deletion", admin, map[string]string{"confirm": org, "reason": "done with it"})
	require.Equal(t, http.StatusAccepted, code, out["_raw"])
	assert.Equal(t, "pending", data(t, out)["state"])
	assert.EqualValues(t, 1, data(t, out)["cancel_links_sent"])

	// suspended at once
	code, _ = h.do(t, http.MethodGet, "/me", admin, nil)
	assert.Equal(t, http.StatusUnauthorized, code)
	code, _ = h.do(t, http.MethodPost, "/operator/organizations/"+org+"/deletion", op, map[string]string{})
	assert.Equal(t, http.StatusConflict, code, "pending already")

	// the administrator cancels with the mailed link
	m := sink.last(t, "hal@example.com")
	assert.Contains(t, m.subject, "will be deleted")
	code, out = h.do(t, http.MethodPost, "/signup/cancel-deletion", "", map[string]string{"token": m.token(t)})
	require.Equal(t, http.StatusOK, code, out["_raw"])
	code, out = h.do(t, http.MethodPost, "/signup/cancel-deletion", "", map[string]string{"token": m.token(t)})
	assert.Equal(t, http.StatusBadRequest, code, "used: %s", out["_raw"])
	code, admin = h.loginPw(t, "hal@example.com", signupPassword)
	require.Equal(t, http.StatusOK, code, "active again")

	// an operator starts it (the administrators get a notice without a link), cancels it, starts it again and
	// expedites it
	code, out = h.do(t, http.MethodPost, "/operator/organizations/"+org+"/deletion", op, map[string]string{"reason": "abuse"})
	require.Equal(t, http.StatusAccepted, code, out["_raw"])
	assert.EqualValues(t, 0, data(t, out)["cancel_links_sent"])
	m = sink.last(t, "hal@example.com")
	assert.Contains(t, m.body, "a platform operator")
	assert.NotContains(t, m.body, "token=")
	code, out = h.do(t, http.MethodDelete, "/operator/organizations/"+org+"/deletion", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	code, _ = h.do(t, http.MethodGet, "/me", admin, nil)
	assert.Equal(t, http.StatusOK, code)
	code, _ = h.do(t, http.MethodPost, "/operator/organizations/"+org+"/deletion", op, map[string]string{})
	require.Equal(t, http.StatusAccepted, code)
	code, out = h.do(t, http.MethodPost, "/operator/organizations/"+org+"/deletion/expedite", op, nil)
	require.Equal(t, http.StatusNoContent, code, out["_raw"])
	code, _ = h.do(t, http.MethodPost, "/operator/organizations/"+org+"/deletion/expedite", op, nil)
	assert.Equal(t, http.StatusNotFound, code, "nothing pending")

	code, _ = h.do(t, http.MethodGet, "/operator/organizations/"+org, op, nil)
	assert.Equal(t, http.StatusNotFound, code)
	var n int
	require.NoError(t, h.db.Owner.Get(&n, "SELECT count(*) FROM users WHERE tenant_id = $1", org))
	assert.Zero(t, n)
	require.NoError(t, h.db.Owner.Get(&n, "SELECT count(*) FROM library_entries WHERE tenant_id = $1", org))
	assert.Zero(t, n)
	require.NoError(t, h.db.Owner.Get(&n, "SELECT count(*) FROM deleted_tenant_ids WHERE id = $1", org))
	assert.Equal(t, 1, n)
	assertNoTenantRows(t, h, org)

	// the id is never given out again
	code, out = h.do(t, http.MethodPost, "/operator/organizations", op, map[string]any{"id": org})
	assert.Equal(t, http.StatusConflict, code, out["_raw"])

	code, out = h.do(t, http.MethodGet, "/operator/deletions", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	var states []string
	for _, d := range out["data"].([]any) {
		states = append(states, d.(map[string]any)["state"].(string))
	}
	assert.Equal(t, []string{"done", "canceled", "canceled"}, states)

	var actions []string
	require.NoError(t, h.db.Owner.Select(&actions, `SELECT action FROM auditlog WHERE tenant_id = '_system'
		AND application = 'organization.deletion' AND affected_id = $1 ORDER BY id`, org))
	assert.Equal(t, []string{"request", "cancel", "request", "cancel", "request", "expedite", "delete"}, actions)
}

// assertNoTenantRows checks that no table with a tenant_id column has rows of org left (except the change feed,
// which is pruned).
func assertNoTenantRows(t *testing.T, h *nodeFixture, org string) {
	t.Helper()
	var tables []string
	require.NoError(t, h.db.Owner.Select(&tables, `SELECT table_name FROM information_schema.columns
		WHERE table_schema = 'public' AND column_name = 'tenant_id' AND table_name <> 'changes'`))
	for _, tbl := range tables {
		var n int
		require.NoError(t, h.db.Owner.Get(&n, fmt.Sprintf("SELECT count(*) FROM %q WHERE tenant_id = $1", tbl), org))
		assert.Zero(t, n, tbl)
	}
}

func signupStore(h *nodeFixture) pgsignup.Store { return pgsignup.Store{DB: h.db.App} }

// fakeShredder records the shredded tenants.
type fakeShredder struct{ shredded []tenant.ID }

func (f *fakeShredder) Shred(_ context.Context, tid tenant.ID) error {
	f.shredded = append(f.shredded, tid)
	return nil
}

// The upkeep deletes an organization once its hold ended, after shredding its vault key, and leaves one an operator
// reactivated meanwhile.
func TestOrganizationDeletionUpkeep(t *testing.T) {
	h, sink := startSignupNode(t, "")
	org1, _ := signUp(t, h, sink, "ida@example.com")
	org2, _ := signUp(t, h, sink, "jo@example.com")
	org3, _ := signUp(t, h, sink, "kim@example.com")
	op := h.login(t, "op")
	for _, o := range []string{org1, org2, org3} {
		code, out := h.do(t, http.MethodPost, "/operator/organizations/"+o+"/deletion", op, map[string]string{})
		require.Equal(t, http.StatusAccepted, code, out["_raw"])
	}
	// org1's hold ended, org2's too but an operator reactivated it, org3's is still running
	_, err := h.db.Owner.Exec("UPDATE org_deletions SET delete_after = now() - interval '1 minute' WHERE organization_id IN ($1, $2)", org1, org2)
	require.NoError(t, err)
	code, out := h.do(t, http.MethodPost, "/operator/organizations/"+org2+"/activate", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])

	shred := &fakeShredder{}
	cfg := loadConfig(t, nodeConf(t, h.db.AppURL, freeAddr(t), nodeSettings{extra: "[signup]\nenabled = true\npublic_url = \"https://ui.example\"\nmail = \"log\"\n"}))
	svc := signup.New(signup.Options{
		Config: cfg.Signup, Store: signupStore(h), Orgs: NewOperator(h.db.App, cfg), Credentials: credentials{db: h.db.App},
		Plans: signup.StandInPlans{}, Mailer: sink, Shredder: shred,
	})
	require.NoError(t, svc.Upkeep(context.Background()))
	assert.Equal(t, []tenant.ID{tenant.ID(org1)}, shred.shredded)

	o := NewOperator(h.db.App, cfg)
	_, err = o.Get(context.Background(), org1)
	assert.True(t, errors.Is(err, operator.ErrNotFound), "%v", err)
	got, err := o.Get(context.Background(), org2)
	require.NoError(t, err)
	assert.Equal(t, operator.StatusActive, got.Status)
	got, err = o.Get(context.Background(), org3)
	require.NoError(t, err)
	assert.Equal(t, operator.StatusSuspended, got.Status)

	code, out = h.do(t, http.MethodGet, "/operator/deletions", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	byOrg := map[string]string{}
	for _, d := range out["data"].([]any) {
		m := d.(map[string]any)
		byOrg[m["organization_id"].(string)] = m["state"].(string)
	}
	assert.Equal(t, map[string]string{org1: "done", org2: "canceled", org3: "pending"}, byOrg)
}

func TestSignupFunctionOnlyForDueDeletions(t *testing.T) {
	h, sink := startSignupNode(t, "")
	org, _ := signUp(t, h, sink, "lee@example.com")
	_, err := h.db.App.Exec("SELECT rport_signup_delete_tenant($1)", org)
	assert.ErrorContains(t, err, "only by platform operators")
	op := h.login(t, "op")
	code, _ := h.do(t, http.MethodPost, "/operator/organizations/"+org+"/deletion", op, map[string]string{})
	require.Equal(t, http.StatusAccepted, code)
	err = signupStore(h).DeleteOrganization(context.Background(), org, "test", time.Now())
	assert.ErrorContains(t, err, "no deletion under way", "pending, not claimed")

	// once claimed, the organization can't be reactivated (its vault key may be gone)
	_, err = h.db.Owner.Exec("UPDATE org_deletions SET state = 'deleting' WHERE organization_id = $1", org)
	require.NoError(t, err)
	_, err = NewOperator(h.db.App, loadConfig(t, nodeConf(t, h.db.AppURL, freeAddr(t), nodeSettings{}))).SetStatus(context.Background(), ops, org, operator.StatusActive)
	assert.ErrorContains(t, err, "is being deleted")
	code, _ = h.do(t, http.MethodPost, "/signup/cancel-deletion", "", map[string]string{"token": "x"})
	assert.Equal(t, http.StatusBadRequest, code)
	code, _ = h.do(t, http.MethodDelete, "/operator/organizations/"+org+"/deletion", op, nil)
	assert.Equal(t, http.StatusNotFound, code, "no pending deletion to cancel")
}

func TestOrganizationExpediteReactivated(t *testing.T) {
	h, sink := startSignupNode(t, "")
	op := h.login(t, "op")
	org, _ := signUp(t, h, sink, "max@example.com")
	code, _ := h.do(t, http.MethodPost, "/operator/organizations/"+org+"/deletion", op, map[string]string{})
	require.Equal(t, http.StatusAccepted, code)
	code, _ = h.do(t, http.MethodPost, "/operator/organizations/"+org+"/activate", op, nil)
	require.Equal(t, http.StatusOK, code)
	code, out := h.do(t, http.MethodPost, "/operator/organizations/"+org+"/deletion/expedite", op, nil)
	assert.Equal(t, http.StatusConflict, code, out["_raw"])
	code, _ = h.do(t, http.MethodGet, "/operator/organizations/"+org, op, nil)
	assert.Equal(t, http.StatusOK, code, "kept")
}

func TestSignupConcurrentApprovals(t *testing.T) {
	h, sink := startSignupNode(t, "require_approval = true\n")
	op := h.login(t, "op")
	code, _ := h.do(t, http.MethodPost, "/signup", "", signupBody("ned@example.com", "Ned"))
	require.Equal(t, http.StatusAccepted, code)
	code, _ = h.do(t, http.MethodPost, "/signup/verify", "", map[string]string{"token": sink.last(t, "ned@example.com").token(t), "password": signupPassword})
	require.Equal(t, http.StatusAccepted, code)
	var id string
	require.NoError(t, h.db.Owner.Get(&id, "SELECT id FROM signups"))
	codes := make(chan int, 4)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _ := h.do(t, http.MethodPost, "/operator/signups/"+id+"/approve", op, nil)
			codes <- c
		}()
	}
	wg.Wait()
	close(codes)
	created := 0
	for c := range codes {
		if c == http.StatusCreated {
			created++
		} else {
			assert.Equal(t, http.StatusConflict, c)
		}
	}
	assert.Equal(t, 1, created)
	var n int
	require.NoError(t, h.db.Owner.Get(&n, "SELECT count(*) FROM deleted_tenant_ids"))
	assert.Zero(t, n, "no organization made and thrown away")
}
