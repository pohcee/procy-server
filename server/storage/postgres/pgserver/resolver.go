package pgserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/bearer"
	"github.com/openrport/openrport/server/routes"
	"github.com/openrport/openrport/share/tenant"
)

// Unresolved is the reserved tenant of requests that carry no credentials naming a tenant. It has no users, so
// authentication fails there with the server's usual 401.
const Unresolved tenant.ID = "_unresolved"

// webSocketAccessTokenParam is where the web UI puts the session token for websocket requests.
const webSocketAccessTokenParam = "access_token"

// maxLoginBody bounds how much of a login request body the resolver reads to find the username.
const maxLoginBody = 64 << 10

var loginPath = routes.AllRoutesPrefix + "/login"

// Resolver finds the tenant of an API request before authentication. It only chooses which tenant's users and
// sessions authentication runs against; it never authenticates anything itself:
//   - a session token (websocket access_token parameter or Authorization: Bearer) names its tenant in the tid claim,
//     read only from a token signed with the server's JWT secret;
//   - basic auth (password or API token) and the username of a login request are looked up in the global login
//     index (logins are unique across tenants);
//   - a tenant that isn't active, and everything else, resolves to Unresolved.
//
// The order follows the server's own authentication: the websocket token first, then basic auth, then the bearer
// token. Credentials of different tenants in one request make authentication fail, never cross tenants.
type Resolver struct {
	db        *sqlx.DB
	jwtSecret string
}

var _ tenant.Resolver = (*Resolver)(nil)

// NewResolver returns the resolver; db is the application connection, jwtSecret the API's JWT secret.
func NewResolver(db *sqlx.DB, jwtSecret string) *Resolver {
	return &Resolver{db: db, jwtSecret: jwtSecret}
}

// Resolve implements tenant.Resolver. It returns an error only when the database can't be read.
func (r *Resolver) Resolve(req *http.Request) (tenant.ID, error) {
	if tok := req.URL.Query().Get(webSocketAccessTokenParam); tok != "" {
		return r.fromToken(req, tok)
	}
	if user, _, ok := req.BasicAuth(); ok {
		return r.fromUsername(req, user)
	}
	if tok, ok := bearer.GetBearerToken(req); ok {
		return r.fromToken(req, tok)
	}
	if req.Method == http.MethodPost && strings.TrimSuffix(req.URL.Path, "/") == loginPath {
		user, err := loginUsername(req)
		if err != nil || user == "" {
			return Unresolved, nil
		}
		return r.fromUsername(req, user)
	}
	return Unresolved, nil
}

func (r *Resolver) fromToken(req *http.Request, tok string) (tenant.ID, error) {
	tc, err := bearer.ParseToken(tok, r.jwtSecret)
	if err != nil || tc.AppClaims.TenantID == "" {
		return Unresolved, nil
	}
	return r.active(req, tc.AppClaims.TenantID)
}

func (r *Resolver) fromUsername(req *http.Request, username string) (tenant.ID, error) {
	if username == "" {
		return Unresolved, nil
	}
	var id string
	err := r.db.GetContext(req.Context(), &id,
		`SELECT l.tenant_id FROM user_logins l JOIN tenants t ON t.id = l.tenant_id
		 WHERE l.username = $1 AND t.status = 'active'`, username)
	if errors.Is(err, sql.ErrNoRows) {
		return Unresolved, nil
	}
	if err != nil {
		return "", fmt.Errorf("pgserver: resolve tenant: %w", err)
	}
	return tenant.ID(id), nil
}

func (r *Resolver) active(req *http.Request, id tenant.ID) (tenant.ID, error) {
	var status string
	err := r.db.GetContext(req.Context(), &status, "SELECT status FROM tenants WHERE id = $1", string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return Unresolved, nil
	}
	if err != nil {
		return "", fmt.Errorf("pgserver: resolve tenant: %w", err)
	}
	if status != "active" {
		return Unresolved, nil
	}
	return id, nil
}

// loginUsername reads the username of a login request the way the login handler does (form or JSON body) and puts
// the body back unchanged for it.
func loginUsername(req *http.Request) (string, error) {
	if req.Body == nil {
		return "", nil
	}
	head, err := io.ReadAll(io.LimitReader(req.Body, maxLoginBody))
	if err != nil {
		return "", err
	}
	req.Body = readCloser{Reader: io.MultiReader(bytes.NewReader(head), req.Body), Closer: req.Body}
	switch req.Header.Get("Content-Type") {
	case "application/x-www-form-urlencoded":
		v, err := url.ParseQuery(string(head))
		if err != nil {
			return "", err
		}
		return v.Get("username"), nil
	case "application/json":
		var body struct {
			Username string `json:"username"`
		}
		if err := json.Unmarshal(head, &body); err != nil {
			return "", err
		}
		return body.Username, nil
	}
	return "", nil
}

type readCloser struct {
	io.Reader
	io.Closer
}
