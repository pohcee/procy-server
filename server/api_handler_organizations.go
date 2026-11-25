package chserver

import (
	"errors"
	"fmt"
	"net/http"

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

type switchOrganizationRequest struct {
	Organization string `json:"organization"`
}

type switchOrganizationResponse struct {
	Token        string `json:"token"`
	Organization string `json:"organization"`
}

// handleSwitchOrganization handles POST /me/organization: a new session token for another organization of the
// current login. The token of the request stays valid in its own organization. Only a session token may switch:
// API tokens (basic auth) are bound to their organization.
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
	tokenStr, err := bearer.CreateAuthToken(target, al.apiSessions, al.config.API.JWTSecret, lifetime, user.Username,
		bearer.ScopesAllExcluding2FaCheck, req.UserAgent(), chshare.RemoteIP(req))
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}
	if err := al.memberships.Remember(target, user.Username); err != nil {
		al.jsonError(w, err)
		return
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(switchOrganizationResponse{Token: tokenStr, Organization: in.Organization}))
}

// handleInviteMember handles POST /organization/members (administrators): adds a login to the organization of the
// request. An existing account becomes a member at once (no acceptance step yet); a new login gets an account with
// a generated password, returned once, that must be changed at the first login.
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
	invited, err := al.memberships.Invite(req.Context(), in)
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
// TOTP secret, name) and the login is a member of other organizations too: only its owner changes those (PUT /me,
// the login's new_password, the TOTP routes). Standalone, and for logins of this organization alone, nothing
// changes.
func (al *APIListener) refuseSharedAccountChange(w http.ResponseWriter, req *http.Request, username string, change *users.User) bool {
	if al.memberships == nil {
		return false
	}
	if change != nil && change.Password == "" && change.PasswordExpired == nil && change.TotP == "" &&
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
		Message:    fmt.Sprintf("%q is a member of other organizations too: only its owner can change its password, two-factor secret or login name", username),
		HTTPStatus: http.StatusForbidden,
	})
	return true
}
