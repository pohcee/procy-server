package chserver

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/pohcee/procy-server/server/oidc"
	"github.com/pohcee/procy-server/server/signup"
	chshare "github.com/pohcee/procy-server/share"
)

// Signup with Google ([signup] and [oidc]; operator console design, Part 3): the Google account's verified email
// replaces the emailed link. GET /oidc/signup?organization=… starts an ordinary OIDC sign-in whose return_to is
// oidcSignupReturn with the organization's name; the callback, seeing it, hands the verified email to the signup
// service instead of signing in (no account is created before an operator approves) and sends the browser to the
// web UI's oidcSignupUIPath with the outcome in the fragment.

const (
	// oidcSignupReturn marks a pending sign-in as a signup; the organization name follows, query-escaped.
	oidcSignupReturn = "/auth/signup/google?organization="
	// oidcSignupUIPath is the page of the web UI a signup with Google ends on.
	oidcSignupUIPath = "/auth/signup/google"
)

// Reasons a signup with Google ends with (#error=…), besides the sign-in's own.
const (
	oidcErrAccountExists = "account_exists"
	oidcErrRateLimited   = "rate_limited"
	oidcErrInvalid       = "invalid"
	oidcErrDomainRefused = "domain_refused"
)

// oidcSignupTarget marks a request whose outcome goes to the signup page (oidcRedirect).
type oidcSignupTarget struct{}

func withOIDCSignupTarget(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), oidcSignupTarget{}, true))
}

// oidcUIURL is the page of the web UI the outcome of req goes to: the sign-in callback page, or the signup page.
func (al *APIListener) oidcUIURL(req *http.Request) string {
	if v, _ := req.Context().Value(oidcSignupTarget{}).(bool); v {
		return strings.TrimRight(al.oidcLogin.cfg.UIURL, "/") + oidcSignupUIPath
	}
	return al.oidcLogin.cfg.UIReturnURL()
}

// signupOrganization returns the organization name of a pending sign-in that is a signup.
func signupOrganization(returnTo string) (string, bool) {
	rest, ok := strings.CutPrefix(returnTo, oidcSignupReturn)
	if !ok {
		return "", false
	}
	org, err := url.QueryUnescape(rest)
	if err != nil {
		return "", false
	}
	return org, true
}

// handleOIDCSignup handles GET /oidc/signup?organization=…: it starts a signup with Google.
func (al *APIListener) handleOIDCSignup(w http.ResponseWriter, req *http.Request) {
	req = withOIDCSignupTarget(req)
	org, err := signup.CheckOrganizationName(req.URL.Query().Get("organization"))
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		al.oidcRedirect(w, req, url.Values{"error": {oidcErrInvalid}, "error_description": {"Enter the name of your organization (1 to 100 characters, no control characters)."}})
		return
	}
	al.startOIDC(w, req, oidcSignupReturn+url.QueryEscape(org))
}

// oidcSignupFinish hands the verified identity of a signup's sign-in to the signup service.
func (al *APIListener) oidcSignupFinish(w http.ResponseWriter, req *http.Request, id *oidc.Identity, org string) {
	if !id.Linkable {
		// the account could never sign in with Google (see oidc.LinkableEmail)
		al.oidcFail(w, req, oidcErrUnmanagedEmail, "Google doesn't manage the address "+id.Email+
			" of this Google account, so it can't sign in with Google. Sign up with the email form instead, or use a Gmail or Google Workspace account.",
			id.Email, "not linkable (signup)")
		return
	}
	res, err := al.signup.RequestVerified(req.Context(), id.Email, org, signup.MethodGoogle, chshare.RemoteIP(req))
	switch {
	case errors.Is(err, signup.ErrAccountExists), errors.Is(err, signup.ErrConflict):
		al.oidcFail(w, req, oidcErrAccountExists, "There is already an account for "+id.Email+". Sign in instead.", id.Email, "signup: "+err.Error())
		return
	case errors.Is(err, signup.ErrRateLimited):
		al.oidcFail(w, req, oidcErrRateLimited, "Too many signups from here today. Try again tomorrow.", id.Email, "signup: rate limited")
		return
	case errors.Is(err, signup.ErrDomain):
		al.oidcFail(w, req, oidcErrDomainRefused, "Signups from this email domain aren't accepted.", id.Email, "signup: domain refused")
		return
	case errors.Is(err, signup.ErrInvalid):
		al.oidcFail(w, req, oidcErrInvalid, "The signup is not valid: "+strings.TrimPrefix(err.Error(), signup.ErrInvalid.Error()+": ")+".", id.Email, "signup: "+err.Error())
		return
	case err != nil:
		al.oidcFail(w, req, oidcErrServer, "The signup failed. Try again later.", id.Email, "signup: "+err.Error())
		return
	}
	params := url.Values{"status": {res.Status}, "organization_name": {res.OrganizationName}, "email": {res.Username}}
	if res.Organization != "" {
		params.Set("organization", res.Organization)
	}
	al.oidcRedirect(w, req, params)
}
