package chserver

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/pohcee/procy-server/server/api/users"

	errors2 "github.com/pohcee/procy-server/server/api/errors"
	"github.com/pohcee/procy-server/server/api/message"
	"github.com/pohcee/procy-server/share/security"
	"github.com/pohcee/procy-server/share/tenant"
)

type TwoFAService struct {
	TokenTTL    time.Duration
	MsgSrv      message.Service
	UserSrv     UserService
	SendTimeout time.Duration

	// tokensByUser holds the pending 2FA codes and TotP login sessions, per tenant and username.
	tokensByUser map[twoFAKey]*expirableToken
	mu           sync.RWMutex
}

type twoFAKey struct {
	tenant   tenant.ID
	username string
}

func twoFAKeyFor(ctx context.Context, username string) (twoFAKey, error) {
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		return twoFAKey{}, fmt.Errorf("2fa: %w", err)
	}
	return twoFAKey{tenant: tid, username: username}, nil
}

func NewTwoFAService(tokenTTLSeconds int, sendTimeout time.Duration, userSrv UserService, msgSrv message.Service) TwoFAService {
	return TwoFAService{
		TokenTTL:     time.Duration(tokenTTLSeconds) * time.Second,
		UserSrv:      userSrv,
		MsgSrv:       msgSrv,
		SendTimeout:  sendTimeout,
		tokensByUser: make(map[twoFAKey]*expirableToken),
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
	key, err := twoFAKeyFor(ctx, username)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, srv.SendTimeout)
	defer cancel()

	if username == "" {
		return "", errors2.APIError{
			Message:    "username cannot be empty",
			HTTPStatus: http.StatusBadRequest,
		}
	}

	user, err := srv.UserSrv.GetByUsername(ctx, username)
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
	srv.tokensByUser[key] = &expirableToken{
		token:  token,
		expiry: time.Now().Add(srv.TokenTTL),
	}
	srv.mu.Unlock()

	return user.TwoFASendTo, nil
}

func (srv *TwoFAService) SetTotPLoginSession(ctx context.Context, username string, loginSessionTTL time.Duration) error {
	key, err := twoFAKeyFor(ctx, username)
	if err != nil {
		return err
	}
	srv.mu.Lock()
	srv.tokensByUser[key] = &expirableToken{
		expiry: time.Now().Add(loginSessionTTL),
	}
	srv.mu.Unlock()
	return nil
}

func (srv *TwoFAService) ValidateTotPCode(ctx context.Context, user *users.User, code string) error {
	key, err := twoFAKeyFor(ctx, user.Username)
	if err != nil {
		return err
	}
	// Write lock for the whole check: a successful code removes the login session below.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	t := srv.tokensByUser[key]

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

	delete(srv.tokensByUser, key)

	return nil
}

// ValidateToken checks a sent 2FA code. A code works once, and is discarded after
// maxTwoFAAttempts wrong guesses or when it expires.
func (srv *TwoFAService) ValidateToken(ctx context.Context, username, token string) error {
	key, err := twoFAKeyFor(ctx, username)
	if err != nil {
		return err
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()

	t := srv.tokensByUser[key]
	if t == nil {
		return errors2.APIError{
			Message:    "2fa token not found for provided username",
			HTTPStatus: http.StatusUnauthorized,
		}
	}

	if time.Now().After(t.expiry) {
		delete(srv.tokensByUser, key)
		return errors2.APIError{
			Message:    "2fa token expired",
			HTTPStatus: http.StatusUnauthorized,
		}
	}

	if subtle.ConstantTimeCompare([]byte(t.token), []byte(token)) != 1 {
		t.attempts++
		if t.attempts >= maxTwoFAAttempts {
			delete(srv.tokensByUser, key)
		}
		return errors2.APIError{
			Message:    "invalid token",
			HTTPStatus: http.StatusUnauthorized,
		}
	}

	delete(srv.tokensByUser, key)
	return nil
}
