package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/api/users"
)

// clientCred is one client authentication credential. Passwords are copied as stored (plaintext in every standalone
// provider); hashing client credentials is left to the servers that read them.
type clientCred struct {
	ID, Password string
}

// srcUser is one API user with what the user providers store about it.
type srcUser struct {
	Username, Password      string
	PasswordExpired         bool
	TwoFASendTo, TotPSecret string
	Groups                  []string
}

// srcGroup is a user group with stored permissions (only the [database] provider stores any).
type srcGroup struct {
	Name, Permissions                     string
	TunnelsRestricted, CommandsRestricted any // JSON text or nil
}

// authSource reads users and client credentials from the auth sources of the source config.
type authSource struct {
	cfg   *sourceConfig
	paths pathResolver
	db    *sourceDB // the [database] SQLite file, when auth tables are used
}

// authDBPath is the SQLite file of [database], when the config reads users or client credentials from its tables.
func (a *authSource) authDBPath() (string, error) {
	if !a.cfg.usesAuthDB() {
		return "", nil
	}
	switch strings.ToLower(a.cfg.Database.Type) {
	case "sqlite", "sqlite3":
	case "":
		return "", errors.New("source config: auth tables are configured but [database] db_type is empty")
	default:
		return "", fmt.Errorf("source config: [database] db_type %q is not supported by the import, only sqlite: copy the auth tables into a SQLite file and point a copy of the config at it", a.cfg.Database.Type)
	}
	if a.cfg.Database.Name == "" {
		return "", errors.New("source config: [database] db_name is empty")
	}
	return a.paths.resolve(a.cfg.Database.Name), nil
}

// clientCreds returns the client credentials, by provider precedence: auth_table, auth_file, auth.
func (a *authSource) clientCreds() ([]clientCred, string, error) {
	s := a.cfg.Server
	switch {
	case s.AuthTable != "":
		var out []clientCred
		err := a.db.db.Select(&out, "SELECT CAST(id AS TEXT) AS id, COALESCE(CAST(password AS TEXT), '') AS password FROM "+quoteIdent(s.AuthTable)+" ORDER BY id")
		if err != nil {
			return nil, "", fmt.Errorf("client auth table %q: %w", s.AuthTable, err)
		}
		return out, "", nil
	case s.AuthFile != "":
		out, err := readClientAuthFile(a.paths.resolve(s.AuthFile))
		return out, "", err
	case s.Auth != "":
		id, pass, ok := strings.Cut(s.Auth, ":")
		if !ok || id == "" || pass == "" {
			return nil, "", errors.New("source config: [server] auth must be <client-auth-id>:<password>")
		}
		return []clientCred{{ID: id, Password: pass}}, "", nil
	}
	return nil, "the source config has no client authentication: no client credentials imported", nil
}

// readClientAuthFile parses {"id": "password", ...}, refusing duplicate ids (which encoding/json would merge).
func readClientAuthFile(path string) ([]clientCred, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("client auth file: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("client auth file %s: expected a JSON object", path)
	}
	var out []clientCred
	seen := map[string]bool{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("client auth file %s: %w", path, err)
		}
		id, _ := kt.(string)
		var pass string
		if err := dec.Decode(&pass); err != nil {
			return nil, fmt.Errorf("client auth file %s: id %q: %w", path, id, err)
		}
		if id == "" || pass == "" {
			return nil, fmt.Errorf("client auth file %s: empty client auth id or password", path)
		}
		if seen[id] {
			return nil, fmt.Errorf("client auth file %s: id %q appears more than once", path, id)
		}
		seen[id] = true
		out = append(out, clientCred{ID: id, Password: pass})
	}
	if _, err := dec.Token(); err != nil && err != io.EOF {
		return nil, fmt.Errorf("client auth file %s: %w", path, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// apiUsers returns the API users and the stored group permissions, by the precedence of the OSS user providers:
// auth_file, auth, auth_user_table.
func (a *authSource) apiUsers() ([]srcUser, []srcGroup, string, error) {
	api := a.cfg.API
	switch {
	case api.AuthFile != "":
		u, err := readUsersFile(a.paths.resolve(api.AuthFile))
		return u, nil, "", err
	case api.Auth != "":
		name, pass, ok := strings.Cut(api.Auth, ":")
		if !ok || name == "" || pass == "" {
			return nil, nil, "", errors.New("source config: [api] auth must be <user>:<password>")
		}
		// the server compares a password without the bcrypt prefix as plaintext; stored users need a hash
		if !strings.HasPrefix(pass, users.HtpasswdBcryptPrefix) {
			h, err := users.GenerateTokenHash(pass)
			if err != nil {
				return nil, nil, "", err
			}
			pass = h
		}
		return []srcUser{{Username: name, Password: pass, Groups: []string{users.Administrators}}}, nil, "", nil
	case api.AuthUserTable != "":
		return a.dbUsers()
	}
	if api.AuthHeader != "" {
		return nil, nil, "the source config authenticates API users by a header only: no users imported", nil
	}
	return nil, nil, "the source config has no API user store: no users imported", nil
}

// readUsersFile reads the [api] auth_file: a JSON array of users with bcrypt password hashes, as the OSS file
// provider requires.
func readUsersFile(path string) ([]srcUser, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("users file: %w", err)
	}
	var file []users.User
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &file); err != nil {
			return nil, fmt.Errorf("users file %s: %w", path, err)
		}
	}
	seen := map[string]bool{}
	out := make([]srcUser, 0, len(file))
	for _, u := range file {
		name := strings.TrimSpace(u.Username)
		pass := strings.TrimSpace(u.Password)
		if name == "" {
			return nil, fmt.Errorf("users file %s: a user has no username", path)
		}
		if !strings.HasPrefix(pass, users.HtpasswdBcryptPrefix) {
			return nil, fmt.Errorf("users file %s: user %q: the password is not a bcrypt hash", path, name)
		}
		if seen[name] {
			return nil, fmt.Errorf("users file %s: user %q appears more than once", path, name)
		}
		seen[name] = true
		out = append(out, srcUser{
			Username: name, Password: pass, PasswordExpired: u.PasswordExpired != nil && *u.PasswordExpired,
			TwoFASendTo: u.TwoFASendTo, TotPSecret: u.TotP, Groups: u.Groups,
		})
	}
	return out, nil
}

// dbUsers reads the [database] users, groups and group details tables (server/api/users/database.go). The optional
// columns are read when present.
func (a *authSource) dbUsers() ([]srcUser, []srcGroup, string, error) {
	api := a.cfg.API
	db := a.db.db
	cols, err := columnsOf(db, api.AuthUserTable)
	if err != nil {
		return nil, nil, "", err
	}
	if len(cols) == 0 {
		return nil, nil, "", fmt.Errorf("users table %q not found in [database]", api.AuthUserTable)
	}
	opt := func(c string) string {
		if cols[c] {
			return "COALESCE(CAST(" + quoteIdent(c) + " AS TEXT), '')"
		}
		return "''"
	}
	expired := "0"
	if cols["password_expired"] {
		expired = "COALESCE(" + quoteIdent("password_expired") + ", 0)"
	}
	var rows []struct {
		Username string `db:"username"`
		Password string `db:"password"`
		Expired  bool   `db:"password_expired"`
		TwoFA    string `db:"two_fa_send_to"`
		TotP     string `db:"totp_secret"`
	}
	q := fmt.Sprintf("SELECT username, COALESCE(password, '') AS password, %s AS password_expired, %s AS two_fa_send_to, %s AS totp_secret FROM %s ORDER BY username",
		expired, opt("two_fa_send_to"), opt("totp_secret"), quoteIdent(api.AuthUserTable))
	if err := db.Select(&rows, q); err != nil {
		return nil, nil, "", fmt.Errorf("users table %q: %w", api.AuthUserTable, err)
	}
	out := make([]srcUser, 0, len(rows))
	byName := map[string]int{}
	for _, r := range rows {
		byName[r.Username] = len(out)
		out = append(out, srcUser{Username: r.Username, Password: r.Password, PasswordExpired: r.Expired, TwoFASendTo: r.TwoFA, TotPSecret: r.TotP})
	}
	var note string
	if api.AuthGroupTable != "" {
		var members []struct {
			Username string `db:"username"`
			Group    string `db:"group"`
		}
		if err := db.Select(&members, "SELECT DISTINCT username, `group` FROM "+quoteIdent(api.AuthGroupTable)+" ORDER BY username, `group`"); err != nil {
			return nil, nil, "", fmt.Errorf("user groups table %q: %w", api.AuthGroupTable, err)
		}
		unknown := 0
		for _, m := range members {
			i, ok := byName[m.Username]
			if !ok {
				unknown++
				continue
			}
			out[i].Groups = append(out[i].Groups, m.Group)
		}
		if unknown > 0 {
			note = fmt.Sprintf("user groups table: %d memberships of unknown users not imported", unknown)
		}
	}
	var groups []srcGroup
	if api.AuthGroupDetailsTable != "" {
		groups, err = readGroupDetails(db, api.AuthGroupDetailsTable)
		if err != nil {
			return nil, nil, "", err
		}
	}
	return out, groups, note, nil
}

func readGroupDetails(db *sqlx.DB, table string) ([]srcGroup, error) {
	cols, err := columnsOf(db, table)
	if err != nil {
		return nil, err
	}
	opt := func(c string) string {
		if cols[c] {
			return "CAST(" + quoteIdent(c) + " AS TEXT)"
		}
		return "NULL"
	}
	var rows []struct {
		Name     string  `db:"name"`
		Perms    *string `db:"permissions"`
		Tunnels  *string `db:"tunnels_restricted"`
		Commands *string `db:"commands_restricted"`
	}
	q := fmt.Sprintf("SELECT name, CAST(permissions AS TEXT) AS permissions, %s AS tunnels_restricted, %s AS commands_restricted FROM %s ORDER BY name",
		opt("tunnels_restricted"), opt("commands_restricted"), quoteIdent(table))
	if err := db.Select(&rows, q); err != nil {
		return nil, fmt.Errorf("group details table %q: %w", table, err)
	}
	out := make([]srcGroup, 0, len(rows))
	for _, r := range rows {
		g := srcGroup{Name: r.Name, Permissions: "{}"}
		if r.Perms != nil && strings.TrimSpace(*r.Perms) != "" {
			g.Permissions = *r.Perms
		}
		if r.Tunnels != nil && strings.TrimSpace(*r.Tunnels) != "" {
			g.TunnelsRestricted = *r.Tunnels
		}
		if r.Commands != nil && strings.TrimSpace(*r.Commands) != "" {
			g.CommandsRestricted = *r.Commands
		}
		out = append(out, g)
	}
	return out, nil
}
