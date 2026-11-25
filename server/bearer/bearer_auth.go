package bearer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v4"

	"github.com/openrport/openrport/server/api/session"
	"github.com/openrport/openrport/server/routes"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

const (
	DefaultMaxTokenLifetime = 90 * 24 * time.Hour
	DefaultTokenLifetime    = 10 * time.Minute
)

type AppTokenClaims struct {
	Username  string  `json:"username,omitempty"`
	SessionID int64   `json:"sessionID,omitempty"`
	Scopes    []Scope `json:"scopes,omitempty"`
	// TenantID is the tenant the session belongs to (#34). Tokens issued before it was added have
	// none; they are valid in the default tenant only.
	TenantID tenant.ID `json:"tid,omitempty"`
	// AMR is how the session signed in (RFC 8176 style): ["oidc"] for an OpenID Connect sign-in. Password
	// sessions (and tokens issued before it was added) have none.
	AMR []string `json:"amr,omitempty"`
	jwt.StandardClaims
}

// AuthMethodOIDC is the AMR value of a session that signed in through OpenID Connect.
const AuthMethodOIDC = "oidc"

type authMethodKey struct{}

// WithAuthMethod returns ctx carrying how the session about to be created signed in (AuthMethodOIDC); a session
// created without it is a password session.
func WithAuthMethod(ctx context.Context, method string) context.Context {
	return context.WithValue(ctx, authMethodKey{}, method)
}

// IsOIDC reports whether the token's session signed in through OpenID Connect.
func (c *AppTokenClaims) IsOIDC() bool {
	if c == nil {
		return false
	}
	for _, m := range c.AMR {
		if m == AuthMethodOIDC {
			return true
		}
	}
	return false
}

type Scope struct {
	URI     string `json:"uri,omitempty"`
	Method  string `json:"method,omitempty"`
	Exclude bool   `json:"exclude,omitempty"`
}

var ScopesAllExcluding2FaCheck = []Scope{
	{
		URI:    "*",
		Method: "*",
	},
	{
		URI:     routes.AllRoutesPrefix + routes.Verify2FaRoute,
		Method:  "*",
		Exclude: true,
	},
}

var ScopesTotPCreateOnly = []Scope{
	{
		URI:    routes.AllRoutesPrefix + routes.TotPRoutes,
		Method: http.MethodPost,
	},
}

var Scopes2FaCheckOnly = []Scope{
	{
		URI:    routes.AllRoutesPrefix + routes.Verify2FaRoute,
		Method: http.MethodPost,
	},
}

type TokenContext struct {
	AppClaims *AppTokenClaims
	RawToken  string
	JwtToken  *jwt.Token
}

type APISessionUpdater interface {
	Save(ctx context.Context, session session.APISession) (sessionID int64, err error)
}

type APISessionGetter interface {
	Get(ctx context.Context, sessionID int64) (found bool, sessionInfo session.APISession, err error)
}

func CreateAuthToken(
	ctx context.Context,
	sessionUpdater APISessionUpdater,
	JWTSecret string,
	lifetime time.Duration,
	username string,
	scopes []Scope,
	userAgent string,
	remoteAddress string,
) (string, error) {
	if username == "" {
		return "", errors.New("username cannot be empty")
	}
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		return "", err
	}

	expiresAt := time.Now().Add(lifetime)

	newSession := session.APISession{
		ExpiresAt:    expiresAt,
		Username:     username,
		LastAccessAt: time.Now(),
		UserAgent:    userAgent,
		IPAddress:    remoteAddress,
	}
	sessionID, err := sessionUpdater.Save(ctx, newSession)
	if err != nil {
		return "", err
	}

	claims := AppTokenClaims{
		Username:  username,
		SessionID: sessionID,
		StandardClaims: jwt.StandardClaims{
			Id: randomTokenID(),
		},
		Scopes:   scopes,
		TenantID: tid,
	}
	if m, _ := ctx.Value(authMethodKey{}).(string); m != "" {
		claims.AMR = []string{m}
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := token.SignedString([]byte(JWTSecret))
	if err != nil {
		return "", err
	}

	return tokenStr, nil
}

func IncreaseSessionLifetime(
	ctx context.Context,
	sessionUpdater APISessionUpdater,
	s session.APISession) error {
	newExpirationDate := time.Now().Add(DefaultTokenLifetime)
	if s.ExpiresAt.Before(newExpirationDate) {
		s.ExpiresAt = newExpirationDate
	}
	_, err := sessionUpdater.Save(ctx, s)
	if err != nil {
		return err
	}
	return nil
}

func currentURIMatchesTokenScopes(currentURI, currentMethod string, tokenScopes []Scope) bool {
	// make it compatible with the old tokens which don't have scopes field in jwt
	if len(tokenScopes) == 0 {
		return true
	}
	currentURI = "/" + strings.Trim(currentURI, "/")

	hasAtLeastOneMatch := false
	hasExcludeMatch := false
	for _, tokenScope := range tokenScopes {
		uriMatched := tokenScope.URI == "*" || currentURI == tokenScope.URI
		methodMatched := tokenScope.Method == "*" || currentMethod == tokenScope.Method

		if uriMatched && methodMatched {
			if tokenScope.Exclude {
				hasExcludeMatch = true
			} else {
				hasAtLeastOneMatch = true
			}
		}
	}

	return hasAtLeastOneMatch && !hasExcludeMatch
}

func ParseToken(tokenStr string, JWTSecret string) (tokCtx *TokenContext, err error) {
	appClaims := &AppTokenClaims{}
	bearerToken, err := jwt.ParseWithClaims(tokenStr, appClaims, func(_ *jwt.Token) (i interface{}, err error) {
		return []byte(JWTSecret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()})) // only HS256 tokens are issued
	if err != nil {
		// do not return error since it should respond with 401 instead of 500, just log it
		return nil, err
	}

	return &TokenContext{
		AppClaims: appClaims,
		RawToken:  tokenStr,
		JwtToken:  bearerToken,
	}, nil
}

// ErrStaleSession: a token this server signed whose login session expired or no longer exists. It is not a
// failed login (a UI left open sends several): callers answer 401 without feeding the brute-force bans.
var ErrStaleSession = errors.New("login session expired")

// IsStale reports whether err (of ParseToken or ValidateBearerToken) is a stale session: ErrStaleSession, or a
// token correctly signed with the secret whose only fault is that it expired (a forged token that also expired has
// the signature flag set too).
func IsStale(err error) bool {
	if errors.Is(err, ErrStaleSession) {
		return true
	}
	var ve *jwt.ValidationError
	return errors.As(err, &ve) && ve.Errors == jwt.ValidationErrorExpired
}

// ValidateBearerToken checks tokCtx against the request and its login session. A session that expired or is gone
// is ErrStaleSession.
func ValidateBearerToken(
	ctx context.Context,
	tokCtx *TokenContext,
	uri, method string,
	apiSessionGetter APISessionGetter,
	l *logger.Logger) (valid bool, sessionInfo session.APISession, err error) {
	if !currentURIMatchesTokenScopes(uri, method, tokCtx.AppClaims.Scopes) {
		l.Errorf(
			"Token scopes %+v don't match with the current url %s[%s], so this token is not intended to be used for this page",
			tokCtx.AppClaims.Scopes,
			method,
			uri,
		)
		return false, session.APISession{}, nil
	}

	if !tokCtx.JwtToken.Valid || tokCtx.AppClaims.Username == "" {
		l.Errorf(
			"Token is invalid or user name is empty",
			tokCtx.AppClaims.Username,
		)
		return false, session.APISession{}, nil
	}

	tid, err := tenant.FromContext(ctx)
	if err != nil {
		return false, session.APISession{}, err
	}
	// A token is valid only in the tenant it was issued in; one without a tenant predates the claim
	// and was issued in the default tenant.
	tokenTenant := tokCtx.AppClaims.TenantID
	if tokenTenant == "" {
		tokenTenant = tenant.Default
	}
	if tokenTenant != tid {
		l.Errorf(
			"Token of %s was issued in another tenant",
			tokCtx.AppClaims.Username,
		)
		return false, session.APISession{}, nil
	}

	found, sessionInfo, err := apiSessionGetter.Get(ctx, tokCtx.AppClaims.SessionID)
	if err != nil {
		l.Errorf("Login session of %s could not be read: %v", tokCtx.AppClaims.Username, err)
		return false, session.APISession{}, err
	}
	if !found {
		l.Debugf("Login session not found for %s", tokCtx.AppClaims.Username)
		return false, session.APISession{}, ErrStaleSession
	}

	// The session must belong to the user named in the token; session IDs are sequential.
	if sessionInfo.Username != tokCtx.AppClaims.Username {
		l.Errorf(
			"Login session %d does not belong to %s",
			tokCtx.AppClaims.SessionID,
			tokCtx.AppClaims.Username,
		)
		return false, session.APISession{}, nil
	}

	isValidByExpirationTime := sessionInfo.ExpiresAt.After(time.Now())
	if !isValidByExpirationTime {
		l.Debugf("api session time %v is expired", sessionInfo.ExpiresAt)
		return false, sessionInfo, ErrStaleSession
	}
	return true, sessionInfo, nil
}

func GetBearerToken(req *http.Request) (string, bool) {
	auth := req.Header.Get("Authorization")
	const prefix = "Bearer "
	// Case insensitive prefix match.
	if len(auth) < len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return "", false
	}
	return auth[len(prefix):], true
}

// randomTokenID returns a random JWT ID from a cryptographically secure source.
func randomTokenID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read doesn't fail on supported platforms
	return hex.EncodeToString(b)
}
