package chserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gorilla/mux"

	rportplus "github.com/pohcee/procy-server/plus"
	extperm "github.com/pohcee/procy-server/plus/capabilities/extendedpermission"
	"github.com/pohcee/procy-server/server/api"
	errors2 "github.com/pohcee/procy-server/server/api/errors"
	"github.com/pohcee/procy-server/server/api/users"
	"github.com/pohcee/procy-server/server/auditlog"
	"github.com/pohcee/procy-server/server/bearer"
	"github.com/pohcee/procy-server/server/oidc"
	"github.com/pohcee/procy-server/server/routes"
)

var (
	ErrMissingUserIDParam    = errors.New("missing user id param")
	ErrMissingSessionIDParam = errors.New("missing session id param")
)

type UserPayload struct {
	Username                     string                       `json:"username"`
	PasswordExpired              bool                         `json:"password_expired"`
	Groups                       []string                     `json:"groups"`
	TwoFASendTo                  string                       `json:"two_fa_send_to"`
	EffectiveUserPermissions     map[string]bool              `json:"effective_user_permissions"`
	EffectiveExtendedPermissions EffectiveExtendedPermissions `json:"effective_extended_permissions"`
	GroupPermissionsEnabled      bool                         `json:"group_permissions_enabled"`
	// EmailVerified is set when signup is on: the login proved to own its mailbox.
	EmailVerified *bool `json:"email_verified,omitempty"`
	// SignIn is the member's sign-in method (both, password, oidc); Postgres only.
	SignIn *oidc.Method `json:"sign_in,omitempty"`
	// OIDCLinked: an OIDC identity is pinned to the account; set with [oidc] enabled.
	OIDCLinked *bool `json:"oidc_linked,omitempty"`
	// AuthMethod is how the request's session signed in (GET /me only): "password" or "oidc".
	AuthMethod string `json:"auth_method,omitempty"`
}

type EffectiveExtendedPermissions struct {
	TunnelsRestricted  []extperm.PermissionParams `json:"tunnels_restricted"`
	CommandsRestricted []extperm.PermissionParams `json:"commands_restricted"`
}

func (al *APIListener) handleGetUsers(w http.ResponseWriter, req *http.Request) {
	usrs, err := al.userService.GetAll(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}

	names := make([]string, 0, len(usrs))
	for _, user := range usrs {
		names = append(names, user.Username)
	}
	details, err := al.signInDetailsOf(req.Context(), names)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	usersToSend := make([]UserPayload, 0, len(usrs))
	for _, user := range usrs {
		payload := UserPayload{
			Username:    user.Username,
			Groups:      user.Groups,
			TwoFASendTo: user.TwoFASendTo,
			SignIn:      details[user.Username].SignIn,
			OIDCLinked:  details[user.Username].OIDCLinked,
		}
		if user.PasswordExpired != nil {
			payload.PasswordExpired = *user.PasswordExpired
		}
		usersToSend = append(usersToSend, payload)
	}

	response := api.NewSuccessPayload(usersToSend)
	al.writeJSONResponse(w, http.StatusOK, response)
}

func (al *APIListener) handleChangeUser(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)

	// if the username is part of the route params then we're updating the user, otherwise creating a new user
	userID, userIDExists := vars[routes.ParamUserID]
	if !userIDExists {
		userID = ""

		err := al.checkUserCount(req.Context())
		if err != nil {
			al.jsonError(w, err)
			return
		}
	}

	var in userChangeRequest
	err := parseRequestBody(req.Body, &in)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	user := in.User
	in.target = userID
	in.authMethod = sessionAuthMethod(req, al.config.API.JWTSecret)

	if userIDExists && al.refuseSharedAccountChange(w, req, userID, &user) {
		return
	}

	method, passwordless, err := al.checkUserSignIn(req.Context(), in, !userIDExists)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	switch {
	case passwordless:
		adder, ok := al.userService.(interface {
			AddWithoutPassword(ctx context.Context, usr *users.User) error
		})
		if !ok {
			al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "password is required")
			return
		}
		err = adder.AddWithoutPassword(req.Context(), &user)
	case userIDExists && method != "" && user.IsEmptyChange():
		// only the sign-in method changes
	default:
		err = al.userService.Change(req.Context(), &user, userID)
	}
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if userIDExists && user.Username != "" && user.Username != userID {
		al.unlinkStandalone(req.Context(), userID)
	}
	if method != "" {
		name := userID
		if user.Username != "" {
			name = user.Username
		}
		if err := al.signInPolicies.SetMethod(req.Context(), name, method); err != nil {
			al.jsonError(w, err)
			return
		}
		al.policyCache.forget()
	}

	if user.PasswordExpired != nil && *user.PasswordExpired {
		// this user password was just set to expired, need to kill all his/her sessions
		ctx := req.Context()
		err := al.apiSessions.DeleteAllByUser(ctx, userID)
		if err != nil {
			titleMsg := fmt.Sprintf("password expired, unable to delete all sessions for user \"%s\"", userID)
			al.jsonErrorResponseWithDetail(w, http.StatusInternalServerError, "Unable to delete all User's sessions", titleMsg, err.Error())
			return
		}
	}

	if userIDExists {
		al.auditLog.Entry(auditlog.ApplicationAuthUser, auditlog.ActionUpdate).
			WithHTTPRequest(req).
			WithID(userID).
			Save()

		al.Debugf("User [%s] updated.", userID)
		w.WriteHeader(http.StatusNoContent)
	} else {
		al.auditLog.Entry(auditlog.ApplicationAuthUser, auditlog.ActionCreate).
			WithHTTPRequest(req).
			Save()

		al.Debugf("User [%s] created.", user.Username)
		w.WriteHeader(http.StatusCreated)
	}
}

func (al *APIListener) checkUserCount(ctx context.Context) (err error) {
	maxUsers := al.getMaxUsers()

	if maxUsers > 0 {
		users, err := al.userService.GetAll(ctx)
		if err != nil {
			return err
		}

		if len(users) >= maxUsers {
			return errors.New("failed to create user. max user limit reached. please upgrade your license for additional users")
		}
	}

	return nil
}

func (al *APIListener) getMaxUsers() (maxUsers int) {
	if rportplus.IsPlusEnabled(al.config.PlusConfig) {
		maxUsers = al.plusManager.GetLicenseCapabilityEx().GetMaxUsers()
	}
	return maxUsers
}

func (al *APIListener) handleDeleteUser(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	userID, userIDExists := vars[routes.ParamUserID]
	if !userIDExists {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "Empty user id provided")
		return
	}

	if err := al.userService.Delete(req.Context(), userID); err != nil {
		al.jsonError(w, err)
		return
	}

	al.unlinkStandalone(req.Context(), userID)

	// A deleted user must not keep access through existing sessions or API tokens.
	if err := al.revokeUserAccess(req.Context(), userID); err != nil {
		al.jsonError(w, err)
		return
	}

	al.auditLog.Entry("user", auditlog.ActionDelete).
		WithHTTPRequest(req).
		WithID(userID).
		Save()

	w.WriteHeader(http.StatusNoContent)
	al.Debugf("User [%s] deleted.", userID)
}

func (al *APIListener) handleDeleteUsersTotP(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	userID, userIDProvided := vars[routes.ParamUserID]
	if !userIDProvided {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "Empty user id provided")
		return
	}

	user, err := al.userService.GetByUsername(req.Context(), userID)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if user == nil {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, "user not found")
		return
	}
	if al.refuseSharedAccountChange(w, req, userID, nil) {
		return
	}

	al.auditLog.Entry(auditlog.ApplicationAuthUserTotP, auditlog.ActionDelete).
		WithHTTPRequest(req).
		WithID(userID).
		Save()

	al.handleManageTotP(w, req, user, "delete")
}

func (al *APIListener) handleGetUserAPISessions(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	userID, userIDExists := vars[routes.ParamUserID]
	if !userIDExists {
		al.jsonError(w, ErrMissingUserIDParam)
		return
	}

	ctx := req.Context()

	userSessions, err := al.apiSessions.GetAllByUser(ctx, userID)
	if err != nil {
		titleMsg := fmt.Sprintf("unable to get sessions for user \"%s\"", userID)
		al.jsonErrorResponseWithDetail(w, http.StatusInternalServerError, "", titleMsg, err.Error())
		return
	}

	response := api.NewSuccessPayload(userSessions)
	al.writeJSONResponse(w, http.StatusOK, response)
}

func (al *APIListener) handleDeleteUserAPISession(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	userID, userIDExists := vars[routes.ParamUserID]
	if !userIDExists {
		al.jsonError(w, ErrMissingUserIDParam)
		return
	}

	sessionID, sessionIDExists := vars[routes.ParamSessionID]
	if !sessionIDExists {
		al.jsonError(w, ErrMissingUserIDParam)
		return
	}

	ctx := req.Context()

	sessID, err := strconv.ParseInt(sessionID, 10, 64)
	if err != nil {
		titleMsg := fmt.Sprintf("unable to parse session ID \"%s\" for user \"%s\"", sessionID, userID)
		al.jsonErrorResponseWithDetail(w, http.StatusInternalServerError, "", titleMsg, err.Error())
		return
	}

	err = al.apiSessions.DeleteByID(ctx, userID, sessID)
	if err != nil {
		titleMsg := fmt.Sprintf("unable to delete session \"%s\" for user \"%s\"", sessionID, userID)
		al.jsonErrorResponseWithDetail(w, http.StatusInternalServerError, "", titleMsg, err.Error())
		return
	}

	al.auditLog.Entry(auditlog.ApplicationAuthAPISession, auditlog.ActionDelete).
		WithHTTPRequest(req).
		WithID(userID).
		Save()

	w.WriteHeader(http.StatusNoContent)
}

func (al *APIListener) handleDeleteAllUserAPISessions(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	userID, userIDExists := vars[routes.ParamUserID]
	if !userIDExists {
		al.jsonError(w, ErrMissingUserIDParam)
		return
	}

	ctx := req.Context()

	err := al.apiSessions.DeleteAllByUser(ctx, userID)
	if err != nil {
		titleMsg := fmt.Sprintf("unable to delete all sessions for user \"%s\"", userID)
		al.jsonErrorResponseWithDetail(w, http.StatusInternalServerError, "", titleMsg, err.Error())
		return
	}

	al.auditLog.Entry(auditlog.ApplicationAuthAPISessions, auditlog.ActionDelete).
		WithHTTPRequest(req).
		WithID(userID).
		Save()

	w.WriteHeader(http.StatusNoContent)
}

// revokeUserAccess deletes all API sessions and API tokens of a user.
func (al *APIListener) revokeUserAccess(ctx context.Context, username string) error {
	if err := al.apiSessions.DeleteAllByUser(ctx, username); err != nil {
		return fmt.Errorf("failed to revoke sessions of %s: %w", username, err)
	}
	tokens, err := al.tokenManager.GetAll(ctx, username)
	if err != nil {
		return fmt.Errorf("failed to list API tokens of %s: %w", username, err)
	}
	for _, t := range tokens {
		if err := al.tokenManager.Delete(ctx, username, t.Prefix); err != nil {
			return fmt.Errorf("failed to revoke API token %s of %s: %w", t.Prefix, username, err)
		}
	}
	return nil
}

// userChangeRequest is the body of POST /users and PUT /users/{user_id}.
type userChangeRequest struct {
	users.User
	// SignIn sets the member's sign-in method (Postgres): both, password or oidc.
	SignIn string `json:"sign_in"`

	// target is the user changed (the route's user id, "" on create); authMethod how the request's session
	// signed in
	target     string `json:"-"`
	authMethod string `json:"-"`
}

// isCurrentUser reports whether the user target (an update) is the request's own login.
func (al *APIListener) isCurrentUser(ctx context.Context, target string) bool {
	return target != "" && api.GetUser(ctx, al.Logger) == target
}

// checkUserSignIn validates the sign-in part of a user change and reports whether a new user is added without a
// password: platform operators never get one (with [oidc] enabled), and a new member who signs in through OIDC only
// (sign_in "oidc", or the organization requires it) needs none.
func (al *APIListener) checkUserSignIn(ctx context.Context, in userChangeRequest, create bool) (oidc.Method, bool, error) {
	var method oidc.Method
	if in.SignIn != "" {
		if al.signInPolicies == nil {
			return "", false, errors2.APIError{HTTPStatus: http.StatusBadRequest, Message: "sign_in is stored only on servers with [storage] backend = \"postgres\""}
		}
		m, err := parseMethod(in.SignIn)
		if err != nil {
			return "", false, err
		}
		if err := al.checkMethod(ctx, m); err != nil {
			return "", false, err
		}
		if m == oidc.MethodOIDC && !create && al.isCurrentUser(ctx, in.target) &&
			in.authMethod != bearer.AuthMethodOIDC {
			return "", false, errors2.APIError{HTTPStatus: http.StatusConflict,
				Message: "sign in with Google before turning password sign-in off for yourself"}
		}
		method = m
	}
	if al.oidcLogin == nil {
		return method, false, nil
	}
	system := al.inSystemTenant(ctx)
	if system && in.Password != "" {
		return "", false, errors2.APIError{HTTPStatus: http.StatusBadRequest, Message: "platform operators sign in with OIDC only: they have no password"}
	}
	if !create || in.Password != "" {
		return method, false, nil
	}
	if system || method == oidc.MethodOIDC {
		return method, true, nil
	}
	if al.signInPolicies != nil {
		require, err := al.signInPolicies.RequireOIDC(ctx)
		if err != nil {
			return "", false, err
		}
		return method, require, nil
	}
	return method, false, nil
}

// unlinkStandalone drops the OIDC identity pinned to a deleted or renamed login of a standalone server, so a later
// login of the same name doesn't inherit it. On Postgres the database does (account_identities follows accounts).
func (al *APIListener) unlinkStandalone(ctx context.Context, username string) {
	if al.oidcLogin == nil || al.memberships != nil {
		return
	}
	if err := al.oidcLogin.store.Unlink(ctx, username); err != nil {
		al.Errorf("oidc: unlink %q: %v", username, err)
	}
}
