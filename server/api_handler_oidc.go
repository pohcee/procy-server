package chserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/openrport/openrport/server/api"
	errors2 "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/server/bearer"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/membership"
	"github.com/openrport/openrport/server/oidc"
	"github.com/openrport/openrport/server/operator"
	chshare "github.com/openrport/openrport/share"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

// OpenID Connect sign-in ([oidc]; docs: oidc.md):
//   - GET /oidc/login starts it: a pending sign-in (state, nonce, PKCE verifier, browser binding) is stored and the
//     browser goes to the provider;
//   - GET /oidc/callback ends it at the provider: the state is taken once, the code exchanged, the ID token
//     verified and mapped to an account, and the browser goes to the web UI with a one-time sign-in code in the
//     fragment (or an error);
//   - POST /oidc/token trades the code for a session, as POST /login does (organization picker included). No TOTP
//     is asked: the provider enforces its own second factor.
//
// Every step reads and writes the shared store, so any node of a cluster handles any step.

const (
	oidcRoutePrefix = "/oidc"
	// oidcCookie binds a pending sign-in to the browser that started it (login CSRF).
	oidcCookie = "procy_oidc"
	// maxReturnTo bounds the path a sign-in returns to.
	maxReturnTo = 512
	// signInPolicyTTL is how long a membership's sign-in policy is cached for password sessions.
	signInPolicyTTL = 15 * time.Second
	// errCodeOIDCRequired is the error code of a password sign-in that is not allowed.
	errCodeOIDCRequired = "oidc_required"
	// auditApplicationOIDC is the audit log application of OIDC sign-ins.
	auditApplicationOIDC = "auth.oidc"
)

// Reasons the callback sends the web UI (#error=…).
const (
	oidcErrAccessDenied     = "access_denied"
	oidcErrInvalidState     = "invalid_state"
	oidcErrEmailUnverified  = "email_unverified"
	oidcErrDomainNotAllowed = "domain_not_allowed"
	oidcErrUnknownAccount   = "unknown_account"
	oidcErrAccountMismatch  = "account_mismatch"
	oidcErrAmbiguous        = "ambiguous_account"
	oidcErrUnmanagedEmail   = "unmanaged_email"
	oidcErrNotAllowed       = "oidc_not_allowed"
	oidcErrServer           = "server_error"
)

// oidcLogin is the OIDC sign-in of an API listener; nil without [oidc] enabled.
type oidcLogin struct {
	cfg      chconfig.OIDCConfig
	provider *oidc.Provider
	store    oidc.Store
	now      func() time.Time
	// cookiePath is the path of the OIDC routes as the browser sees them (from redirect_url: a proxy may add a
	// prefix), where the binding cookie is sent
	cookiePath string
}

func newOIDCLogin(cfg chconfig.OIDCConfig, store oidc.Store, client *http.Client) (*oidcLogin, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if store == nil {
		return nil, errors.New("[oidc] is enabled but the storage has no OIDC store")
	}
	cookiePath := oidcRoutePrefix
	if u, err := url.Parse(cfg.RedirectURL); err == nil {
		cookiePath = strings.TrimSuffix(u.Path, "/callback")
	}
	return &oidcLogin{cfg: cfg, provider: oidc.NewProvider(cfg, client), store: store, now: time.Now, cookiePath: cookiePath}, nil
}

// initOIDC sets up OIDC sign-in ([oidc]) on the listener's storage; standalone, the email of a sign-in is matched
// against the API users.
func (al *APIListener) initOIDC(repos *Repositories, client *http.Client) error {
	o, err := newOIDCLogin(al.config.OIDC, repos.OIDC, client)
	if err != nil {
		return err
	}
	al.oidcLogin = o
	if s, ok := repos.OIDC.(interface {
		SetLogins(func(context.Context) ([]string, error))
	}); ok {
		s.SetLogins(func(ctx context.Context) ([]string, error) {
			all, err := al.userService.GetAll(ctx)
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(all))
			for _, u := range all {
				names = append(names, u.Username)
			}
			return names, nil
		})
	}
	al.logOIDC()
	return nil
}

// signInPolicyCache caches the sign-in policy of memberships for password sessions (bearer tokens are checked on
// every request).
type signInPolicyCache struct {
	mu sync.Mutex
	m  map[string]cachedPolicy
}

type cachedPolicy struct {
	p   oidc.Policy
	exp time.Time
}

func (c *signInPolicyCache) get(key string, now time.Time) (oidc.Policy, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || now.After(e.exp) {
		return oidc.Policy{}, false
	}
	return e.p, true
}

func (c *signInPolicyCache) put(key string, p oidc.Policy, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) > 10000 {
		c.m = map[string]cachedPolicy{}
	}
	c.m[key] = cachedPolicy{p: p, exp: now.Add(signInPolicyTTL)}
}

func (c *signInPolicyCache) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = nil
}

// signInPolicy returns the sign-in policy of username in the tenant of ctx. Without [oidc] nothing is enforced
// (the zero policy allows both); standalone servers (no policy store) enforce nothing but [oidc] itself.
func (al *APIListener) signInPolicy(ctx context.Context, username string) (oidc.Policy, error) {
	if al.oidcLogin == nil {
		return oidc.Policy{}, nil
	}
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		return oidc.Policy{}, err
	}
	p := oidc.Policy{}
	if al.signInPolicies != nil {
		if p, err = al.signInPolicies.Policy(ctx, username); err != nil {
			return oidc.Policy{}, err
		}
	}
	p.System = tid == operator.SystemTenant
	return p, nil
}

// cachedSignInPolicy is signInPolicy, cached for signInPolicyTTL.
func (al *APIListener) cachedSignInPolicy(ctx context.Context, username string) (oidc.Policy, error) {
	if al.oidcLogin == nil {
		return oidc.Policy{}, nil
	}
	now := time.Now()
	key := userBanKey(ctx, username)
	if p, ok := al.policyCache.get(key, now); ok {
		return p, nil
	}
	p, err := al.signInPolicy(ctx, username)
	if err != nil {
		return p, err
	}
	al.policyCache.put(key, p, now)
	return p, nil
}

// errOIDCRequired answers a password sign-in that the membership's policy refuses.
func (al *APIListener) errOIDCRequired() error {
	label := "your organization's single sign-on"
	if al.oidcLogin != nil {
		label = "“" + al.oidcLogin.cfg.ButtonLabel + "”"
	}
	return errors2.APIError{HTTPStatus: http.StatusForbidden, ErrCode: errCodeOIDCRequired,
		Message: "Password sign-in is turned off for this account: use " + label + "."}
}

// respondSignInError answers err, keeping its error code (oidc_required) in the payload.
func (al *APIListener) respondSignInError(w http.ResponseWriter, err error) {
	var ae errors2.APIError
	if errors.As(err, &ae) && ae.ErrCode != "" {
		al.jsonErrorResponseWithErrCode(w, ae.HTTPStatus, ae.ErrCode, ae.Message)
		return
	}
	al.jsonError(w, err)
}

// passwordAllowed checks that username may sign in with a password in the tenant of ctx.
func (al *APIListener) passwordAllowed(ctx context.Context, username string) error {
	p, err := al.signInPolicy(ctx, username)
	if err != nil {
		return err
	}
	if !p.PasswordAllowed() {
		return al.errOIDCRequired()
	}
	return nil
}

// sessionAllowed reports whether a session (bearer token) of username that signed in as claims says may still be
// used in the tenant of ctx: a password session stops when its organization (or the member) requires OIDC.
func (al *APIListener) sessionAllowed(ctx context.Context, claims *bearer.AppTokenClaims) (bool, error) {
	if al.oidcLogin == nil || claims.IsOIDC() {
		return true, nil
	}
	p, err := al.cachedSignInPolicy(ctx, claims.Username)
	if err != nil {
		return false, err
	}
	return p.PasswordAllowed(), nil
}

// safeReturnTo keeps a path of the web UI only: absolute, not to another host ("//host", "/\host").
func safeReturnTo(s string) string {
	if s == "" || len(s) > maxReturnTo || !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") ||
		strings.HasPrefix(s, "/\\") || strings.ContainsAny(s, "\r\n") {
		return ""
	}
	return s
}

// oidcRedirect sends the browser to the web UI's sign-in page with params in the fragment.
func (al *APIListener) oidcRedirect(w http.ResponseWriter, req *http.Request, params url.Values) {
	http.Redirect(w, req, al.oidcLogin.cfg.UIReturnURL()+"#"+params.Encode(), http.StatusFound)
}

// oidcFail ends a sign-in at the web UI with reason; who and detail go to the log and the audit log.
func (al *APIListener) oidcFail(w http.ResponseWriter, req *http.Request, reason, description, who, detail string) {
	al.Infof("oidc: sign-in failed (%s) for %q from %s: %s", reason, who, chshare.RemoteIP(req), detail)
	ctx := req.Context()
	if al.memberships != nil {
		// a failed sign-in belongs to no organization yet: the platform operators' audit log has it
		ctx = tenant.WithID(ctx, operator.SystemTenant)
	}
	al.auditOIDC(req.WithContext(api.WithUser(ctx, who)), "failed", reason)
	al.oidcRedirect(w, req, url.Values{"error": {reason}, "error_description": {description}})
}

func (al *APIListener) auditOIDC(req *http.Request, action, id string) {
	al.auditLog.Entry(auditApplicationOIDC, action).WithHTTPRequest(req).WithID(id).Save()
}

func (al *APIListener) oidcSetCookie(w http.ResponseWriter, value string, maxAge int) {
	//nolint:gosec // G124: HttpOnly and SameSite are set; Secure is set whenever the server is reached over https
	http.SetCookie(w, &http.Cookie{
		Name:     oidcCookie,
		Value:    value,
		Path:     al.oidcLogin.cookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   strings.HasPrefix(al.oidcLogin.cfg.RedirectURL, "https://"),
		// the callback is a top-level navigation from the provider: Lax cookies are sent with it
		SameSite: http.SameSiteLaxMode,
	})
}

// handleOIDCLogin handles GET /oidc/login?return_to=/path: it starts a sign-in and redirects to the provider.
func (al *APIListener) handleOIDCLogin(w http.ResponseWriter, req *http.Request) {
	o := al.oidcLogin
	state, err1 := oidc.RandomToken()
	nonce, err2 := oidc.RandomToken()
	binding, err3 := oidc.RandomToken()
	if err := errors.Join(err1, err2, err3); err != nil {
		al.jsonError(w, err)
		return
	}
	verifier := oauth2.GenerateVerifier()
	authURL, err := o.provider.AuthURL(state, nonce, verifier)
	if err != nil {
		al.oidcFail(w, req, oidcErrServer, "The sign-in provider can't be reached. Try again later.", "", err.Error())
		return
	}
	err = o.store.SaveLogin(req.Context(), oidc.Login{
		StateHash:   oidc.Hash(state),
		Nonce:       nonce,
		Verifier:    verifier,
		BindingHash: oidc.Hash(binding),
		ReturnTo:    safeReturnTo(req.URL.Query().Get("return_to")),
		ExpiresAt:   o.now().Add(oidc.LoginTTL),
	})
	if err != nil {
		al.oidcFail(w, req, oidcErrServer, "The sign-in could not be started. Try again later.", "", err.Error())
		return
	}
	al.oidcSetCookie(w, binding, int(oidc.LoginTTL/time.Second))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, req, authURL, http.StatusFound)
}

// handleOIDCCallback handles GET /oidc/callback: the provider's redirect at the end of a sign-in. It never feeds
// the password ban: a failed callback is not a guessed password.
func (al *APIListener) handleOIDCCallback(w http.ResponseWriter, req *http.Request) {
	o := al.oidcLogin
	ctx := req.Context()
	q := req.URL.Query()
	w.Header().Set("Cache-Control", "no-store")

	var login *oidc.Login
	var takeErr error = oidc.ErrNotFound
	if state := q.Get("state"); state != "" {
		login, takeErr = o.store.TakeLogin(ctx, oidc.Hash(state), o.now())
	}
	cookie, cookieErr := req.Cookie(oidcCookie)
	// the cookie is cleared unless the sign-in succeeds (then it binds the code until POST /oidc/token)
	al.oidcSetCookie(w, "", -1)

	if e := q.Get("error"); e != "" {
		reason := oidcErrServer
		if e == "access_denied" {
			reason = oidcErrAccessDenied
		}
		al.oidcFail(w, req, reason, "The sign-in was canceled or refused at the provider.", "", e+": "+q.Get("error_description"))
		return
	}
	if takeErr != nil {
		if !errors.Is(takeErr, oidc.ErrNotFound) {
			al.oidcFail(w, req, oidcErrServer, "The sign-in failed. Try again later.", "", takeErr.Error())
			return
		}
		// no audit entry: anyone can send made-up states; the IP ban counts them
		al.handleBannedIPs(req, false)
		al.Infof("oidc: callback with an unknown, expired or replayed state from %s", chshare.RemoteIP(req))
		al.oidcRedirect(w, req, url.Values{"error": {oidcErrInvalidState}, "error_description": {"The sign-in expired or was used already. Start again."}})
		return
	}
	if cookieErr != nil || subtle.ConstantTimeCompare([]byte(oidc.Hash(cookie.Value)), []byte(login.BindingHash)) != 1 {
		al.oidcFail(w, req, oidcErrInvalidState, "The sign-in was started in another browser. Start again.", "", "browser binding cookie missing or wrong")
		return
	}

	id, err := o.provider.Exchange(ctx, q.Get("code"), login.Verifier, login.Nonce)
	if errors.Is(err, oidc.ErrNonce) {
		al.oidcFail(w, req, oidcErrInvalidState, "The sign-in could not be verified. Start again.", "", err.Error())
		return
	}
	if err != nil {
		al.oidcFail(w, req, oidcErrServer, "The sign-in could not be verified. Try again later.", "", err.Error())
		return
	}
	if !id.EmailVerified || id.Email == "" {
		al.oidcFail(w, req, oidcErrEmailUnverified, "The email address of this account is not verified at the provider.", id.Email, "email not verified")
		return
	}
	if !o.cfg.DomainAllowed(id.Email, id.HostedDomain) {
		al.oidcFail(w, req, oidcErrDomainNotAllowed, "Accounts of this domain can't sign in here.", id.Email, "domain not allowed, hd="+id.HostedDomain)
		return
	}
	id.Linkable = oidc.LinkableEmail(*id)
	username, err := o.store.Resolve(ctx, *id)
	switch {
	case errors.Is(err, oidc.ErrNotLinkable):
		al.oidcFail(w, req, oidcErrUnmanagedEmail, "Google doesn't manage the address "+id.Email+
			" of this Google account, so it can't be linked. Sign in with your password, or use a Gmail or Google Workspace account.", id.Email, "not linkable")
		return
	case errors.Is(err, oidc.ErrUnknownAccount):
		al.oidcFail(w, req, oidcErrUnknownAccount, "There is no account for "+id.Email+". Ask your administrator to add you, or sign up.", id.Email, "no account")
		return
	case errors.Is(err, oidc.ErrAmbiguous):
		al.oidcFail(w, req, oidcErrAmbiguous, "Several accounts match "+id.Email+". Ask your administrator.", id.Email, "ambiguous")
		return
	case errors.Is(err, oidc.ErrMismatch):
		al.oidcFail(w, req, oidcErrAccountMismatch, "The account "+id.Email+" is linked to a different Google account.", id.Email,
			"pinned to another subject; this one is "+id.Subject)
		return
	case err != nil:
		al.oidcFail(w, req, oidcErrServer, "The sign-in failed. Try again later.", id.Email, err.Error())
		return
	}
	orgs, err := al.oidcOrganizations(ctx, username)
	if err != nil {
		al.oidcFail(w, req, oidcErrServer, "The sign-in failed. Try again later.", username, err.Error())
		return
	}
	if len(orgs) == 0 {
		al.oidcFail(w, req, oidcErrNotAllowed, "This account doesn't sign in with Google here: use your password.", username, "no organization allows it")
		return
	}
	code, err := oidc.RandomToken()
	if err == nil {
		err = o.store.SaveTicket(ctx, oidc.Ticket{CodeHash: oidc.Hash(code), Username: username, BindingHash: login.BindingHash,
			ExpiresAt: o.now().Add(oidc.TicketTTL)})
	}
	if err != nil {
		al.oidcFail(w, req, oidcErrServer, "The sign-in failed. Try again later.", username, err.Error())
		return
	}
	// the same browser trades the code: the binding cookie stays until then
	al.oidcSetCookie(w, cookie.Value, int(oidc.TicketTTL/time.Second))
	params := url.Values{"code": {code}}
	if login.ReturnTo != "" {
		params.Set("return_to", login.ReturnTo)
	}
	al.oidcRedirect(w, req, params)
}

// oidcOrganizations returns the organizations username may sign in to through OIDC. Standalone: the server's own
// tenant (the store matched the email against its users).
func (al *APIListener) oidcOrganizations(ctx context.Context, username string) ([]membership.Organization, error) {
	if al.memberships == nil {
		tid, err := tenant.FromContext(ctx)
		if err != nil {
			return nil, err
		}
		u, err := al.userService.GetByUsername(ctx, username)
		if err != nil || u == nil {
			return nil, err
		}
		return []membership.Organization{{ID: string(tid)}}, nil
	}
	orgs, err := al.memberships.Organizations(ctx, username)
	if err != nil {
		return nil, err
	}
	out := []membership.Organization{}
	for _, org := range orgs {
		p, err := al.signInPolicy(tenant.WithID(ctx, tenant.ID(org.ID)), username)
		if err != nil {
			return nil, err
		}
		if p.OIDCAllowed() {
			out = append(out, org)
		}
	}
	return out, nil
}

type oidcTokenRequest struct {
	Code         string `json:"code"`
	Organization string `json:"organization"`
}

// handleOIDCToken handles POST /oidc/token: the web UI trades the sign-in code of the callback for a session. The
// answer is the one of POST /login; a login of several organizations naming none gets their list and no token, and
// the code stays valid for the call that names one.
func (al *APIListener) handleOIDCToken(w http.ResponseWriter, req *http.Request) {
	o := al.oidcLogin
	ctx := req.Context()
	var in oidcTokenRequest
	if err := parseRequestBody(req.Body, &in); err != nil {
		al.jsonError(w, err)
		return
	}
	if in.Organization == "" {
		in.Organization = req.URL.Query().Get(organizationParam)
	}
	codeHash := oidc.Hash(in.Code)
	t, err := o.store.GetTicket(ctx, codeHash, o.now())
	switch {
	case errors.Is(err, oidc.ErrNotFound):
		// a code that never existed is guessed: it counts for the IP ban
		al.handleBannedIPs(req, false)
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "invalid sign-in code")
		return
	case errors.Is(err, oidc.ErrExpired):
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "the sign-in code expired or was used already: sign in again")
		return
	case err != nil:
		al.jsonError(w, err)
		return
	}
	// login CSRF: a code is traded only by the browser that started its sign-in
	if c, err := req.Cookie(oidcCookie); err != nil || subtle.ConstantTimeCompare([]byte(oidc.Hash(c.Value)), []byte(t.BindingHash)) != 1 {
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "the sign-in was started in another browser: sign in again")
		return
	}
	orgs, err := al.oidcOrganizations(ctx, t.Username)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if len(orgs) == 0 {
		al.jsonErrorResponseWithTitle(w, http.StatusForbidden, "this account doesn't sign in with Google here")
		return
	}
	var tid tenant.ID
	switch {
	case al.memberships == nil:
		tid = tenant.ID(orgs[0].ID)
	case in.Organization == "" && len(orgs) > 1:
		al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(loginResponse{Organizations: orgs}))
		return
	case in.Organization == "":
		tid = tenant.ID(orgs[0].ID)
	default:
		for _, org := range orgs {
			if org.ID == in.Organization {
				tid = tenant.ID(org.ID)
			}
		}
		if tid == "" {
			al.jsonErrorResponseWithTitle(w, http.StatusForbidden, fmt.Sprintf("you can't sign in to %q with Google", in.Organization))
			return
		}
	}
	lifetime, err := parseTokenLifetime(req)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusBadRequest, err)
		return
	}
	if err := o.store.UseTicket(ctx, codeHash, o.now()); err != nil {
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "the sign-in code expired or was used already: sign in again")
		return
	}
	tctx := bearer.WithAuthMethod(tenant.WithID(ctx, tid), bearer.AuthMethodOIDC)
	user, err := al.userService.GetByUsername(tctx, t.Username)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if user == nil {
		al.jsonErrorResponseWithTitle(w, http.StatusForbidden, "the account is no longer a member there")
		return
	}
	tokenStr, err := bearer.CreateAuthToken(tctx, al.apiSessions, al.config.API.JWTSecret, lifetime, t.Username,
		bearer.ScopesAllExcluding2FaCheck, req.UserAgent(), chshare.RemoteIP(req))
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}
	al.auditOIDC(req.WithContext(api.WithUser(tctx, t.Username)), "success", t.Username)
	al.oidcSetCookie(w, "", -1)
	resp := loginResponse{Token: &tokenStr}
	if al.memberships != nil && len(orgs) > 1 {
		current := string(tid)
		resp.Organization = &current
		resp.Organizations = orgs
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(resp))
}

// oidcProviderInfo is what the login page needs to show the OIDC button (no secrets).
type oidcProviderInfo struct {
	Enabled     bool   `json:"enabled"`
	ButtonLabel string `json:"button_label"`
	LoginURL    string `json:"login_url"`
}

func (al *APIListener) oidcInfo() *oidcProviderInfo {
	if al.oidcLogin == nil {
		return nil
	}
	return &oidcProviderInfo{Enabled: true, ButtonLabel: al.oidcLogin.cfg.ButtonLabel, LoginURL: "/api/v1" + oidcRoutePrefix + "/login"}
}

// logOIDC logs the provider at startup.
func (al *APIListener) logOIDC() {
	if al.oidcLogin != nil {
		al.Logf(logger.LogLevelInfo, "OIDC sign-in enabled: issuer %s, callback %s", al.oidcLogin.cfg.Issuer, al.oidcLogin.cfg.RedirectURL)
	}
}
