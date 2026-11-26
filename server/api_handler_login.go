package chserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	rportplus "github.com/pohcee/procy-server/plus"
	"github.com/pohcee/procy-server/server/api"
	errors2 "github.com/pohcee/procy-server/server/api/errors"
	"github.com/pohcee/procy-server/server/api/users"
	"github.com/pohcee/procy-server/server/auditlog"
	"github.com/pohcee/procy-server/server/bearer"
	"github.com/pohcee/procy-server/server/membership"
	chshare "github.com/pohcee/procy-server/share"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/tenant"
)

type twoFAResponse struct {
	SendTo         string `json:"send_to"`
	DeliveryMethod string `json:"delivery_method"`
	TotPKeyStatus  string `json:"totp_key_status"`
}

type loginResponse struct {
	Token *string        `json:"token"`  // null if 2fa is on
	TwoFA *twoFAResponse `json:"two_fa"` // null if 2fa is off
	// Organization and Organizations are set only for a login that belongs to several organizations (Postgres):
	// the organization the token is for, and every organization the login may switch to (POST /me/organization) or
	// name at login ("organization"). A login of several that names none gets Organizations and no token.
	Organization  *string                   `json:"organization,omitempty"`
	Organizations []membership.Organization `json:"organizations,omitempty"`
}

// withOrganizations adds the organizations of username to resp when it belongs to several. Standalone: resp
// unchanged.
func (al *APIListener) withOrganizations(ctx context.Context, username string, resp loginResponse) (loginResponse, error) {
	if al.memberships == nil {
		return resp, nil
	}
	orgs, err := al.memberships.Organizations(ctx, username)
	if err != nil {
		return resp, err
	}
	if len(orgs) < 2 {
		return resp, nil
	}
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		return resp, err
	}
	current := string(tid)
	resp.Organization = &current
	resp.Organizations = orgs
	return resp, nil
}

// organizationParam names the organization of a login (query parameter, or a field of the POST body).
const organizationParam = "organization"

// answerOrganizationChoice answers the login of username with its organizations and no token, and returns true,
// when it belongs to several and the request names none (Postgres). Otherwise it does nothing.
func (al *APIListener) answerOrganizationChoice(w http.ResponseWriter, req *http.Request, username string) bool {
	if al.memberships == nil || req.URL.Query().Get(organizationParam) != "" {
		return false
	}
	orgs, err := al.memberships.Organizations(req.Context(), username)
	if err != nil {
		al.jsonError(w, err)
		return true
	}
	if len(orgs) < 2 {
		return false
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(loginResponse{Organizations: orgs}))
	return true
}

// writeLoginResponse answers a login with resp and the organizations of username.
func (al *APIListener) writeLoginResponse(w http.ResponseWriter, req *http.Request, username string, resp loginResponse) {
	resp, err := al.withOrganizations(req.Context(), username, resp)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(resp))
}

func (al *APIListener) handleGetLogin(w http.ResponseWriter, req *http.Request) {
	if rportplus.IsPlusOAuthEnabled(al.config.PlusConfig) {
		al.jsonErrorResponse(w, http.StatusForbidden, errors.New("built-in authorization disabled. please authorize via your configured authorization"))
		return
	}

	if al.config.API.AuthHeader != "" && req.Header.Get(al.config.API.AuthHeader) != "" {
		// The header is only meaningful when a trusted reverse proxy set it; anyone else could send it.
		if chshare.IsTrustedPeer(req) {
			al.handleLogin(req.Header.Get(al.config.API.UserHeader), "", "", true /* skipPasswordValidation */, w, req)
			return
		}
		al.Infof("ignoring %s header from %s: not a trusted proxy", al.config.API.AuthHeader, req.RemoteAddr)
	}

	basicUser, basicPwd, basicAuthProvided := req.BasicAuth()
	if basicAuthProvided {
		al.handleLogin(basicUser, basicPwd, "", false, w, req)
		return
	}

	// TODO: consider to move this check from all API endpoints to middleware similar to https://github.com/realvnc-labs/rport/pull/199/commits/4ca1ca9f56c557762d79a60ffc96d2de47f3133c
	// ban IP if it sends a lot of bad requests
	if !al.handleBannedIPs(req, false) {
		return
	}
	al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "auth is required")
}

func (al *APIListener) handleLogin(username, pwd string, newpwd string, skipPasswordValidation bool, w http.ResponseWriter, req *http.Request) {
	if al.isUserBanned(req.Context(), username) {
		al.jsonErrorResponseWithTitle(w, http.StatusTooManyRequests, ErrTooManyRequests.Error())
		return
	}

	if username == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "username is required")
		return
	}

	authorized, user, err := al.validateCredentials(req.Context(), username, pwd, skipPasswordValidation)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	if !al.handleBannedIPs(req, authorized) {
		return
	}

	if !authorized {
		if user != nil {
			// a wrong password of a login of this tenant; unknown logins are not audited
			al.auditLogin(req, username, auditlog.ActionFailed)
		}
		al.banUser(req.Context(), username)
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	lifetime, err := parseTokenLifetime(req)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusBadRequest, err)
		return
	}

	// A login of several organizations that names none gets their list and no token, before any 2FA or password
	// change: the client repeats the login with the organization query parameter ("organization").
	if done := al.answerOrganizationChoice(w, req, username); done {
		return
	}

	// the organization (or the member) may require OIDC sign-in; a trusted proxy's header login is no password
	if !skipPasswordValidation {
		if err := al.passwordAllowed(req.Context(), username); err != nil {
			al.respondSignInError(w, err)
			return
		}
	}

	// Only set the new password after the old password has been verified.
	if newpwd != "" {
		if err := al.userService.Change(
			req.Context(),
			&users.User{
				Password:        newpwd,
				PasswordExpired: users.PasswordExpired(false)},
			username); err != nil {
			al.jsonError(w, err)
			return
		}
		user.PasswordExpired = users.PasswordExpired(false) // from here on
	}

	if user.PasswordExpired != nil && *user.PasswordExpired {
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, ErrThatPasswordHasExpired.Error())
		return
	}

	if al.config.API.IsTwoFAOn() {
		sendTo, err := al.twoFASrv.SendToken(req.Context(), username, req.UserAgent(), chshare.RemoteIP(req))
		if err != nil {
			al.jsonError(w, err)
			return
		}

		// 2fa token
		tokenStr, err := bearer.CreateAuthToken(
			req.Context(),
			al.apiSessions,
			al.config.API.JWTSecret,
			lifetime,
			username,
			bearer.Scopes2FaCheckOnly,
			req.UserAgent(),
			chshare.RemoteIP(req),
		)
		if err != nil {
			al.jsonErrorResponse(w, http.StatusInternalServerError, err)
			return
		}

		al.writeLoginResponse(w, req, username, loginResponse{
			Token: &tokenStr,
			TwoFA: &twoFAResponse{
				SendTo:         sendTo,
				DeliveryMethod: al.twoFASrv.MsgSrv.DeliveryMethod(),
			},
		})
		return
	}

	if al.config.API.TotPEnabled {
		if err := al.twoFASrv.SetTotPLoginSession(req.Context(), username, al.config.API.TotPLoginSessionTimeout); err != nil {
			al.jsonError(w, err)
			return
		}

		loginResp := loginResponse{
			TwoFA: &twoFAResponse{
				DeliveryMethod: "totp_authenticator_app",
			},
		}

		totP, err := GetUsersTotPCode(user)
		if err != nil {
			al.Logf(logger.LogLevelError, "failed to get TotP secret: %v", err)
			al.jsonErrorResponse(w, http.StatusInternalServerError, err)
			return
		}

		scopes := bearer.Scopes2FaCheckOnly
		if totP == nil {
			// we allow access to totp-secret creation only if no totp secret was created before
			scopes = append(scopes, bearer.ScopesTotPCreateOnly...)
			loginResp.TwoFA.TotPKeyStatus = TotPKeyPending.String()
		} else {
			loginResp.TwoFA.TotPKeyStatus = TotPKeyExists.String()
		}

		// TotP token
		tokenStr, err := bearer.CreateAuthToken(
			req.Context(),
			al.apiSessions,
			al.config.API.JWTSecret,
			lifetime,
			username,
			scopes,
			req.UserAgent(),
			chshare.RemoteIP(req),
		)
		if err != nil {
			al.jsonErrorResponse(w, http.StatusInternalServerError, err)
			return
		}

		loginResp.Token = &tokenStr
		al.writeLoginResponse(w, req, username, loginResp)
		return
	}

	// login token, normal
	tokenStr, err := bearer.CreateAuthToken(
		req.Context(),
		al.apiSessions,
		al.config.API.JWTSecret,
		lifetime,
		username,
		bearer.ScopesAllExcluding2FaCheck,
		req.UserAgent(),
		chshare.RemoteIP(req),
	)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}
	al.passwordLoginSucceeded(req, username)

	al.writeLoginResponse(w, req, username, loginResponse{Token: &tokenStr})
}

func (al *APIListener) sendJWTToken(username string, w http.ResponseWriter, req *http.Request) {
	lifetime, err := parseTokenLifetime(req)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusBadRequest, err)
		return
	}

	// login token, after 2fa
	tokenStr, err := bearer.CreateAuthToken(
		req.Context(),
		al.apiSessions,
		al.config.API.JWTSecret,
		lifetime,
		username,
		bearer.ScopesAllExcluding2FaCheck,
		req.UserAgent(),
		chshare.RemoteIP(req),
	)
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}
	al.passwordLoginSucceeded(req, username)

	al.writeLoginResponse(w, req, username, loginResponse{Token: &tokenStr})
}

func (al *APIListener) handlePostLogin(w http.ResponseWriter, req *http.Request) {
	if rportplus.IsPlusOAuthEnabled(al.config.PlusConfig) {
		al.jsonErrorResponse(w, http.StatusForbidden, errors.New("built-in authorization disabled. please authorize via your configured authorization"))
		return
	}

	// updating the Password via newPassword field is allowed with a POST or PATCH request
	username, pwd, newPassword, org, err := parseLoginRequestBody(req)

	if err != nil {
		// ban IP if it sends a lot of bad requests
		if !al.handleBannedIPs(req, false) {
			return
		}
		al.jsonError(w, err)
		return
	}

	if org != "" && req.URL.Query().Get(organizationParam) == "" {
		// the organization may come in the body too; handleLogin reads it from the query
		q := req.URL.Query()
		q.Set(organizationParam, org)
		req.URL.RawQuery = q.Encode()
	}
	al.handleLogin(username, pwd, newPassword, false, w, req)
}

func parseLoginRequestBody(req *http.Request) (username, password, newPassword, organization string, _ error) {
	reqContentType := req.Header.Get("Content-Type")
	if reqContentType == "application/x-www-form-urlencoded" {
		err := req.ParseForm()
		if err != nil {
			return "", "", "", "", errors2.APIError{
				Err:        fmt.Errorf("failed to parse form: %v", err),
				HTTPStatus: http.StatusBadRequest,
			}
		}
		return req.PostForm.Get("username"), req.PostForm.Get("password"), req.PostForm.Get("new_password"), req.PostForm.Get(organizationParam), nil
	}
	if reqContentType == "application/json" {
		type loginReq struct {
			Username    string `json:"username"`
			Password    string `json:"password"`
			NewPassword string `json:"new_password"`
			// Organization picks the organization of a login in several (Postgres), like the query parameter
			Organization string `json:"organization"`
		}
		var params loginReq
		err := parseRequestBody(req.Body, &params)
		if err != nil {
			return "", "", "", "", err
		}
		return params.Username, params.Password, params.NewPassword, params.Organization, nil
	}
	return "", "", "", "", errors2.APIError{
		Message:    fmt.Sprintf("unsupported content type: %s", reqContentType),
		HTTPStatus: http.StatusBadRequest,
	}
}

func parseTokenLifetime(req *http.Request) (time.Duration, error) {
	lifetimeStr := req.URL.Query().Get("token-lifetime")
	if lifetimeStr == "" {
		lifetimeStr = "0"
	}
	lifetime, err := strconv.ParseInt(lifetimeStr, 10, 0)
	if err != nil {
		return 0, fmt.Errorf("invalid token-lifetime : %s", err)
	}
	result := time.Duration(lifetime) * time.Second
	if result > bearer.DefaultMaxTokenLifetime {
		return 0, fmt.Errorf("requested token lifetime exceeds max allowed %d", bearer.DefaultMaxTokenLifetime/time.Second)
	}
	if result <= 0 {
		result = bearer.DefaultTokenLifetime
	}
	return result, nil
}
