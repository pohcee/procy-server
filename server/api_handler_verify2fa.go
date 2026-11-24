package chserver

import (
	"net/http"

	"github.com/openrport/openrport/server/api"
	errors2 "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/server/bearer"
)

func (al *APIListener) handlePostVerify2FAToken() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		username, err := al.parseAndValidate2FATokenRequest(req)
		if err != nil {
			if !al.handleBannedIPs(req, false) {
				return
			}
			al.Errorf(err.Error())
			al.jsonError(w, err)
			return
		}

		al.sendJWTToken(username, w, req)
	})
}

func (al *APIListener) parseAndValidate2FATokenRequest(req *http.Request) (username string, err error) {
	if !al.config.API.IsTwoFAOn() && !al.config.API.TotPEnabled {
		return "", errors2.APIError{
			HTTPStatus: http.StatusConflict,
			Message:    "2fa is disabled",
		}
	}

	var reqBody struct {
		Username string `json:"username"`
		Token    string `json:"token"`
	}
	err = parseRequestBody(req.Body, &reqBody)
	if err != nil {
		return "", err
	}

	if al.isUserBanned(req.Context(), reqBody.Username) {
		return reqBody.Username, errors2.APIError{
			HTTPStatus: http.StatusTooManyRequests,
			Err:        ErrTooManyRequests,
		}
	}

	if reqBody.Username == "" {
		return "", errors2.APIError{
			HTTPStatus: http.StatusUnauthorized,
			Message:    "username is required",
		}
	}

	if reqBody.Token == "" {
		return reqBody.Username, errors2.APIError{
			HTTPStatus: http.StatusUnauthorized,
			Message:    "token is required",
		}
	}

	if al.config.API.TotPEnabled {
		bearerToken, bearerAuthProvided := bearer.GetBearerToken(req)

		if !bearerAuthProvided {
			return reqBody.Username, errors2.APIError{
				HTTPStatus: http.StatusBadRequest,
				Message:    "token is required",
			}
		}

		isAuthorized, token, err := al.checkBearerToken(req.Context(), bearerToken, req.URL.Path, req.Method)
		if err != nil {
			return reqBody.Username, err
		}

		if !isAuthorized {
			return reqBody.Username, errors2.APIError{
				HTTPStatus: http.StatusForbidden,
				Message:    "access denied",
			}
		}

		user, err := al.userService.GetByUsername(req.Context(), token.AppClaims.Username)
		if err != nil {
			return "", err
		}
		return token.AppClaims.Username, al.twoFASrv.ValidateTotPCode(req.Context(), user, reqBody.Token)
	}

	// The code must verify the login that the scoped token belongs to, not a username from the body.
	username = api.GetUser(req.Context(), al.Logger)
	if username == "" {
		return "", errors2.APIError{
			HTTPStatus: http.StatusUnauthorized,
			Message:    "login session required",
		}
	}
	return username, al.twoFASrv.ValidateToken(req.Context(), username, reqBody.Token)
}
