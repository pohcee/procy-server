package chserver

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/openrport/openrport/server/api/users"

	errors2 "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/server/api/message"
	"github.com/openrport/openrport/share/security"
)

type TwoFAService struct {
	TokenTTL    time.Duration
	MsgSrv      message.Service
	UserSrv     UserService
	SendTimeout time.Duration

	tokensByUser map[string]*expirableToken
	mu           sync.RWMutex
}

func NewTwoFAService(tokenTTLSeconds int, sendTimeout time.Duration, userSrv UserService, msgSrv message.Service) TwoFAService {
	return TwoFAService{
		TokenTTL:     time.Duration(tokenTTLSeconds) * time.Second,
		UserSrv:      userSrv,
		MsgSrv:       msgSrv,
		SendTimeout:  sendTimeout,
		tokensByUser: make(map[string]*expirableToken),
	}
}

type expirableToken struct {
	token    string
	expiry   time.Time
	attempts int
}

const twoFATokenLength = 6

// maxTwoFAAttempts is how many wrong codes are accepted before a sent code is discarded.
const maxTwoFAAttempts = 5

func (srv *TwoFAService) SendToken(ctx context.Context, username string, userAgent string, remoteAddress string) (sendTo string, err error) {
	ctx, cancel := context.WithTimeout(ctx, srv.SendTimeout)
	defer cancel()

	if username == "" {
		return "", errors2.APIError{
			Message:    "username cannot be empty",
			HTTPStatus: http.StatusBadRequest,
		}
	}

	user, err := srv.UserSrv.GetByUsername(username)
	if err != nil {
		return "", err
	}
	if user == nil {
		return "", errors2.APIError{
			Message:    fmt.Sprintf("user with username %s not found", username),
			HTTPStatus: http.StatusNotFound,
		}
	}

	if user.TwoFASendTo == "" {
		return "", errors2.APIError{
			Message:    "no two_fa_send_to set for this user",
			HTTPStatus: http.StatusBadRequest,
		}
	}

	token, err := security.NewRandomToken(twoFATokenLength)
	if err != nil {
		return "", fmt.Errorf("failed to generate 2fa token: %wv", err)
	}

	data := message.Data{
		SendTo:        user.TwoFASendTo,
		Token:         token,
		TTL:           srv.TokenTTL,
		Title:         "🔐 RRort Two-Factor Token",
		RemoteAddress: remoteAddress,
		UserAgent:     userAgent,
	}
	if err := srv.MsgSrv.Send(ctx, data); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return "", fmt.Errorf("failed to send 2fa verification code: %w", err)
	}

	srv.mu.Lock()
	srv.tokensByUser[username] = &expirableToken{
		token:  token,
		expiry: time.Now().Add(srv.TokenTTL),
	}
	srv.mu.Unlock()

	return user.TwoFASendTo, nil
}

func (srv *TwoFAService) SetTotPLoginSession(username string, loginSessionTTL time.Duration) {
	srv.mu.Lock()
	srv.tokensByUser[username] = &expirableToken{
		expiry: time.Now().Add(loginSessionTTL),
	}
	srv.mu.Unlock()
}

func (srv *TwoFAService) ValidateTotPCode(user *users.User, code string) error {
	// Write lock for the whole check: a successful code removes the login session below.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	t := srv.tokensByUser[user.Username]

	if t == nil {
		return errors2.APIError{
			Message:    "login request not found for provided username",
			HTTPStatus: http.StatusUnauthorized,
		}
	}

	if time.Now().After(t.expiry) {
		return errors2.APIError{
			Message:    "login request expired",
			HTTPStatus: http.StatusUnauthorized,
		}
	}

	totP, err := GetUsersTotPCode(user)
	if err != nil {
		return errors2.APIError{
			Err:        err,
			HTTPStatus: http.StatusInternalServerError,
		}
	}
	if totP == nil || totP.Secret == "" {
		return errors2.APIError{
			Message:    "time based one time secret key should be generated for this user",
			HTTPStatus: http.StatusConflict,
		}
	}

	if !CheckTotPCode(code, totP) {
		return errors2.APIError{
			Message:    "invalid code",
			HTTPStatus: http.StatusUnauthorized,
		}
	}

	delete(srv.tokensByUser, user.Username)

	return nil
}

// ValidateToken checks a sent 2FA code. A code works once, and is discarded after
// maxTwoFAAttempts wrong guesses or when it expires.
func (srv *TwoFAService) ValidateToken(username, token string) error {
	srv.mu.Lock()
	defer srv.mu.Unlock()

	t := srv.tokensByUser[username]
	if t == nil {
		return errors2.APIError{
			Message:    "2fa token not found for provided username",
			HTTPStatus: http.StatusUnauthorized,
		}
	}

	if time.Now().After(t.expiry) {
		delete(srv.tokensByUser, username)
		return errors2.APIError{
			Message:    "2fa token expired",
			HTTPStatus: http.StatusUnauthorized,
		}
	}

	if subtle.ConstantTimeCompare([]byte(t.token), []byte(token)) != 1 {
		t.attempts++
		if t.attempts >= maxTwoFAAttempts {
			delete(srv.tokensByUser, username)
		}
		return errors2.APIError{
			Message:    "invalid token",
			HTTPStatus: http.StatusUnauthorized,
		}
	}

	delete(srv.tokensByUser, username)
	return nil
}
