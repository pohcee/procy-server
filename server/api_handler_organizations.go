package chserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/openrport/openrport/server/api"
	errors2 "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/auditlog"
	"github.com/openrport/openrport/server/bearer"
	"github.com/openrport/openrport/server/membership"
	chshare "github.com/openrport/openrport/share"
	"github.com/openrport/openrport/share/tenant"
)

// The organization routes exist only with APIListenerOptions.Memberships (Postgres nodes).

type myOrganization struct {
	membership.Organization
	// Current: the organization of the request's session
	Current bool `json:"current"`
}

// handleGetMyOrganizations handles GET /me/organizations: the active organizations of the current login.
func (al *APIListener) handleGetMyOrganizations(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	user, err := al.getUserModelForAuth(ctx)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	orgs, err := al.memberships.Organizations(ctx, user.Username)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	out := make([]myOrganization, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, myOrganization{Organization: o, Current: o.ID == string(tid)})
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(out))
}

// handleGetMyOrganization handles GET /me/organization: the organization of the request's session.
func (al *APIListener) handleGetMyOrganization(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	user, err := al.getUserModelForAuth(ctx)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	orgs, err := al.memberships.Organizations(ctx, user.Username)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	for _, o := range orgs {
		if o.ID == string(tid) {
			al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(o))
			return
		}
	}
	al.jsonErrorResponseWithTitle(w, http.StatusNotFound, "the login is not a member of an organization")
}

type switchOrganizationRequest struct {
	Organization string `json:"organization"`
}

type switchOrganizationResponse struct {
	Token        string `json:"token"`
	Organization string `json:"organization"`
}

// handleSwitchOrganization handles POST /me/organization: a new session token for another organization of the
// current login. The token of the request stays valid in its own organization. Only a session token may switch:
// API tokens (basic auth) are bound to their organization. No new 2FA: the account's 2FA address and TOTP secret
// are the same in every organization, and the session passed them.
func (al *APIListener) handleSwitchOrganization(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	if _, _, basic := req.BasicAuth(); basic {
		al.jsonErrorResponseWithTitle(w, http.StatusForbidden, "switching organizations needs a session token: API tokens and passwords sent with each request are bound to one organization")
		return
	}
	var in switchOrganizationRequest
	if err := parseRequestBody(req.Body, &in); err != nil {
		al.jsonError(w, err)
		return
	}
	if in.Organization == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "organization is required")
		return
	}
	lifetime, err := parseTokenLifetime(req)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusBadRequest, err)
		return
	}
	user, err := al.getUserModelForAuth(ctx)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	orgs, err := al.memberships.Organizations(ctx, user.Username)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	member := false
	for _, o := range orgs {
		member = member || o.ID == in.Organization
	}
	if !member {
		// the same answer whether the organization exists or not
		al.jsonErrorResponseWithTitle(w, http.StatusForbidden, fmt.Sprintf("you are not a member of an active organization %q", in.Organization))
		return
	}
	target := tenant.WithID(ctx, tenant.ID(in.Organization))
	// the new session signed in as the current one did, and the target organization's sign-in policy applies
	policy, err := al.signInPolicy(target, user.Username)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if sessionAuthMethod(req, al.config.API.JWTSecret) == bearer.AuthMethodOIDC {
		if !policy.OIDCAllowed() {
			al.jsonErrorResponseWithTitle(w, http.StatusForbidden, fmt.Sprintf("you can't use Google sign-in in %q: sign in with your password", in.Organization))
			return
		}
		target = bearer.WithAuthMethod(target, bearer.AuthMethodOIDC)
	} else if !policy.PasswordAllowed() {
		al.respondSignInError(w, al.errOIDCRequired())
		return
	}
	tokenStr, err := bearer.CreateAuthToken(target, al.apiSessions, al.config.API.JWTSecret, lifetime, user.Username,
		bearer.ScopesAllExcluding2FaCheck, req.UserAgent(), chshare.RemoteIP(req))
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(switchOrganizationResponse{Token: tokenStr, Organization: in.Organization}))
}

// handleInviteMember handles POST /organization/members (administrators): adds a login to the organization of the
// request. An existing account gets a pending invitation that its owner accepts (POST /me/invitations/{org}); a
// new login gets an account with a generated password, returned once, that must be changed at the first login.
func (al *APIListener) handleInviteMember(w http.ResponseWriter, req *http.Request) {
	var in membership.Invitation
	if err := parseRequestBody(req.Body, &in); err != nil {
		al.jsonError(w, err)
		return
	}
	if err := al.checkUserCount(req.Context()); err != nil {
		al.jsonError(w, err)
		return
	}
	by, err := al.getUserModelForAuth(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if al.oidcLogin != nil && al.signInPolicies != nil {
		// an organization that requires OIDC sign-in gives new logins no password
		if in.NoPassword, err = al.signInPolicies.RequireOIDC(req.Context()); err != nil {
			al.jsonError(w, err)
			return
		}
	}
	invited, err := al.memberships.Invite(req.Context(), in, by.Username)
	switch {
	case errors.Is(err, membership.ErrAlreadyMember):
		al.jsonErrorResponseWithTitle(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, membership.ErrInvalid):
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		al.jsonError(w, err)
		return
	}
	al.auditLog.Entry(auditlog.ApplicationAuthUser, auditlog.ActionCreate).
		WithHTTPRequest(req).
		WithID(in.Username).
		Save()
	al.writeJSONResponse(w, http.StatusCreated, api.NewSuccessPayload(invited))
}

// refuseSharedAccountChange answers 403 and returns true when an administrator's change of the user username
// would change what belongs to its account rather than to this organization's membership (password, its expiry,
// 2FA address, TOTP secret, name) and the login is a member of other organizations too: only its owner changes those (PUT /me,
// the login's new_password, the TOTP routes). Standalone, and for logins of this organization alone, nothing
// changes.
func (al *APIListener) refuseSharedAccountChange(w http.ResponseWriter, req *http.Request, username string, change *users.User) bool {
	if al.memberships == nil {
		return false
	}
	if change != nil && change.Password == "" && change.PasswordExpired == nil && change.TotP == "" && change.TwoFASendTo == "" &&
		(change.Username == "" || change.Username == username) {
		return false
	}
	shared, err := al.memberships.MemberElsewhere(req.Context(), username)
	if err != nil {
		al.jsonError(w, err)
		return true
	}
	if !shared {
		return false
	}
	al.jsonError(w, errors2.APIError{
		Message:    fmt.Sprintf("%q is a member of other organizations too: only its owner can change its password, two-factor settings or login name", username),
		HTTPStatus: http.StatusForbidden,
	})
	return true
}

// handleGetMyInvitations handles GET /me/invitations: the pending invitations of the current login.
func (al *APIListener) handleGetMyInvitations(w http.ResponseWriter, req *http.Request) {
	user, err := al.getUserModelForAuth(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}
	invs, err := al.memberships.Invitations(req.Context(), user.Username)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(invs))
}

// myInvitationTarget returns the context of the organization of the current login's pending invitation named in
// the route, or answers and returns false. accept needs a session token (a membership of another organization
// must not be made with a token bound to one).
func (al *APIListener) myInvitationTarget(w http.ResponseWriter, req *http.Request, accept bool) (context.Context, *users.User, bool) {
	if _, _, basic := req.BasicAuth(); basic && accept {
		al.jsonErrorResponseWithTitle(w, http.StatusForbidden, "accepting an invitation needs a session token: API tokens and passwords sent with each request are bound to one organization")
		return nil, nil, false
	}
	user, err := al.getUserModelForAuth(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return nil, nil, false
	}
	org := mux.Vars(req)["organization"]
	invs, err := al.memberships.Invitations(req.Context(), user.Username)
	if err != nil {
		al.jsonError(w, err)
		return nil, nil, false
	}
	for _, i := range invs {
		if i.ID == org {
			return tenant.WithID(req.Context(), tenant.ID(org)), user, true
		}
	}
	al.jsonErrorResponseWithTitle(w, http.StatusNotFound, fmt.Sprintf("no pending invitation from an active organization %q", org))
	return nil, nil, false
}

// handleAcceptInvitation handles POST /me/invitations/{organization}: the current login becomes a member of the
// organization with the groups of the invitation. Switch to it with POST /me/organization.
func (al *APIListener) handleAcceptInvitation(w http.ResponseWriter, req *http.Request) {
	target, user, ok := al.myInvitationTarget(w, req, true)
	if !ok {
		return
	}
	switch err := al.memberships.Accept(target, user.Username); {
	case errors.Is(err, membership.ErrNoInvitation):
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, membership.ErrAlreadyMember):
		al.jsonErrorResponseWithTitle(w, http.StatusConflict, err.Error())
	case errors.Is(err, membership.ErrInvalid):
		al.jsonErrorResponseWithTitle(w, http.StatusForbidden, err.Error())
		return
	case err != nil:
		al.jsonError(w, err)
		return
	}
	org, _ := tenant.FromContext(target)
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(membership.Organization{ID: string(org)}))
}

// handleDeclineInvitation handles DELETE /me/invitations/{organization}.
func (al *APIListener) handleDeclineInvitation(w http.ResponseWriter, req *http.Request) {
	target, user, ok := al.myInvitationTarget(w, req, false)
	if !ok {
		return
	}
	if err := al.memberships.Withdraw(target, user.Username); err != nil && !errors.Is(err, membership.ErrNoInvitation) {
		al.jsonError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGetPendingInvitations handles GET /organization/invitations (administrators).
func (al *APIListener) handleGetPendingInvitations(w http.ResponseWriter, req *http.Request) {
	invs, err := al.memberships.PendingInvitations(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(invs))
}

// handleWithdrawInvitation handles DELETE /organization/invitations/{username} (administrators).
func (al *APIListener) handleWithdrawInvitation(w http.ResponseWriter, req *http.Request) {
	username := mux.Vars(req)["username"]
	switch err := al.memberships.Withdraw(req.Context(), username); {
	case errors.Is(err, membership.ErrNoInvitation):
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		al.jsonError(w, err)
		return
	}
	al.auditLog.Entry(auditlog.ApplicationAuthUser, auditlog.ActionDelete).
		WithHTTPRequest(req).
		WithID(username).
		Save()
	w.WriteHeader(http.StatusNoContent)
}
