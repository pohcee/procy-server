package chserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/openrport/openrport/server/api"
	errors2 "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/server/auditlog"
	"github.com/openrport/openrport/server/bearer"
	"github.com/openrport/openrport/server/oidc"
	"github.com/openrport/openrport/server/operator"
	"github.com/openrport/openrport/server/routes"
	"github.com/openrport/openrport/share/tenant"
)

// The sign-in settings: an organization's OIDC requirement (administrators), the sign-in method of members
// (administrators, and members themselves) and the identity pinned to an account. The settings are stored on
// Postgres only (signInPolicies); standalone users always may use both.

// auditApplicationSignIn is the audit log application of sign-in setting changes.
const auditApplicationSignIn = "auth.signin"

type orgSignIn struct {
	RequireOIDC bool `json:"require_oidc"`
	OIDCEnabled bool `json:"oidc_enabled"`
}

func (al *APIListener) inSystemTenant(ctx context.Context) bool {
	tid, err := tenant.FromContext(ctx)
	return err == nil && tid == operator.SystemTenant
}

// handleGetOrganizationSignIn handles GET /organization/sign-in.
func (al *APIListener) handleGetOrganizationSignIn(w http.ResponseWriter, req *http.Request) {
	require, err := al.signInPolicies.RequireOIDC(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(orgSignIn{RequireOIDC: require || al.inSystemTenant(req.Context()),
		OIDCEnabled: al.oidcLogin != nil}))
}

// handlePutOrganizationSignIn handles PUT /organization/sign-in {"require_oidc": bool}. Requiring OIDC needs [oidc]
// enabled, or nobody could sign in.
func (al *APIListener) handlePutOrganizationSignIn(w http.ResponseWriter, req *http.Request) {
	var in struct {
		RequireOIDC bool `json:"require_oidc"`
	}
	if err := parseRequestBody(req.Body, &in); err != nil {
		al.jsonError(w, err)
		return
	}
	if in.RequireOIDC && al.oidcLogin == nil {
		al.jsonErrorResponseWithTitle(w, http.StatusConflict, "OIDC sign-in is not enabled on this server ([oidc])")
		return
	}
	if err := al.signInPolicies.SetRequireOIDC(req.Context(), in.RequireOIDC); err != nil {
		al.jsonError(w, err)
		return
	}
	al.policyCache.forget()
	al.auditLog.Entry(auditApplicationSignIn, auditlog.ActionUpdate).WithHTTPRequest(req).WithRequest(in).Save()
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(orgSignIn{RequireOIDC: in.RequireOIDC, OIDCEnabled: al.oidcLogin != nil}))
}

// checkMethod validates the sign-in method m for username in the tenant of ctx: OIDC only needs [oidc] enabled,
// and password only is refused where OIDC is required.
func (al *APIListener) checkMethod(ctx context.Context, m oidc.Method) error {
	if m == oidc.MethodOIDC && al.oidcLogin == nil {
		return errors2.APIError{HTTPStatus: http.StatusConflict, Message: "OIDC sign-in is not enabled on this server ([oidc])"}
	}
	if m == oidc.MethodPassword {
		if al.inSystemTenant(ctx) {
			return errors2.APIError{HTTPStatus: http.StatusBadRequest, Message: "platform operators sign in with OIDC only"}
		}
		require, err := al.signInPolicies.RequireOIDC(ctx)
		if err != nil {
			return err
		}
		if require {
			return errors2.APIError{HTTPStatus: http.StatusBadRequest, Message: "the organization requires OIDC sign-in"}
		}
	}
	return nil
}

func parseMethod(s string) (oidc.Method, error) {
	m, err := oidc.ParseMethod(s)
	if err != nil {
		return "", errors2.APIError{HTTPStatus: http.StatusBadRequest, Message: err.Error()}
	}
	return m, nil
}

// handlePutMeSignIn handles PUT /me/sign-in {"method": "both"|"password"|"oidc"}: a member's own choice. OIDC only
// needs an identity pinned (the member signed in through OIDC once), so nobody locks themselves out.
func (al *APIListener) handlePutMeSignIn(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	var in struct {
		Method string `json:"method"`
	}
	if err := parseRequestBody(req.Body, &in); err != nil {
		al.jsonError(w, err)
		return
	}
	m, err := parseMethod(in.Method)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	user, err := al.getUserModelForAuth(ctx)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if err := al.checkMethod(ctx, m); err != nil {
		al.jsonError(w, err)
		return
	}
	if m == oidc.MethodOIDC {
		linked, err := al.oidcLogin.store.Linked(ctx, []string{user.Username})
		if err != nil {
			al.jsonError(w, err)
			return
		}
		if !linked[user.Username] {
			al.jsonErrorResponseWithTitle(w, http.StatusConflict, "sign in with Google once before turning password sign-in off")
			return
		}
	}
	if err := al.signInPolicies.SetMethod(ctx, user.Username, m); err != nil {
		al.jsonError(w, err)
		return
	}
	al.policyCache.forget()
	al.auditLog.Entry(auditApplicationSignIn, auditlog.ActionUpdate).WithHTTPRequest(req).WithID(user.Username).WithRequest(in).Save()
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteUserOIDCIdentity handles DELETE /users/{user_id}/oidc-identity: unpins the identity of a member's
// account (its next OIDC sign-in pins again). Refused for an account shared with other organizations.
func (al *APIListener) handleDeleteUserOIDCIdentity(w http.ResponseWriter, req *http.Request) {
	username := mux.Vars(req)[routes.ParamUserID]
	user, err := al.userService.GetByUsername(req.Context(), username)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if user == nil {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, "user not found")
		return
	}
	if al.refuseSharedAccountChange(w, req, username, nil) {
		return
	}
	err = al.oidcLogin.store.Unlink(req.Context(), username)
	if errors.Is(err, oidc.ErrNoSuchUser) {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		al.jsonError(w, err)
		return
	}
	al.auditLog.Entry(auditApplicationOIDC, auditlog.ActionDelete).WithHTTPRequest(req).WithID(username).Save()
	w.WriteHeader(http.StatusNoContent)
}

// signInDetails are the sign-in fields of a user in GET /users and GET /me; nil fields are left out.
type signInDetails struct {
	SignIn     *oidc.Method
	OIDCLinked *bool
}

// signInDetailsOf returns the sign-in fields of usernames in the tenant of ctx.
func (al *APIListener) signInDetailsOf(ctx context.Context, usernames []string) (map[string]signInDetails, error) {
	out := map[string]signInDetails{}
	var methods map[string]oidc.Method
	if al.signInPolicies != nil {
		var err error
		if methods, err = al.signInPolicies.Methods(ctx); err != nil {
			return nil, err
		}
	}
	var linked map[string]bool
	if al.oidcLogin != nil {
		var err error
		if linked, err = al.oidcLogin.store.Linked(ctx, usernames); err != nil {
			return nil, err
		}
	}
	for _, u := range usernames {
		d := signInDetails{}
		if methods != nil {
			m := methods[u]
			if m == "" {
				m = oidc.MethodBoth
			}
			d.SignIn = &m
		}
		if linked != nil {
			l := linked[u]
			d.OIDCLinked = &l
		}
		out[u] = d
	}
	return out, nil
}

// sessionAuthMethod is how the request's session signed in: "oidc" or "password".
func sessionAuthMethod(req *http.Request, jwtSecret string) string {
	if tok, ok := bearer.GetBearerToken(req); ok {
		if tc, err := bearer.ParseToken(tok, jwtSecret); err == nil && tc.AppClaims.IsOIDC() {
			return bearer.AuthMethodOIDC
		}
	}
	return "password"
}
