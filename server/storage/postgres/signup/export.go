package signup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/signup"
	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/share/tenant"
)

// exportQueries read each section of an export as one JSON array. Secret columns are never selected (passwords,
// TOTP secrets, API tokens, client credentials, vault values); scrub removes secrets nested in JSON documents.
var exportQueries = []struct {
	name string
	sql  string
}{
	{"users", `SELECT u.username, coalesce((SELECT a.two_fa_send_to FROM accounts a WHERE a.username = u.username), '') AS two_fa_send_to,
		coalesce((SELECT json_agg(g.group_name ORDER BY g.group_name) FROM user_groups g WHERE g.tenant_id = u.tenant_id AND g.username = u.username), '[]') AS groups
		FROM users u WHERE u.tenant_id = $1 ORDER BY u.username`},
	{"user_groups", `SELECT name, permissions::json AS permissions, tunnels_restricted::json AS tunnels_restricted,
		commands_restricted::json AS commands_restricted FROM groups WHERE tenant_id = $1 ORDER BY name`},
	{"clients", `SELECT id, client_auth_id, disconnected_at, details::json AS details FROM clients WHERE tenant_id = $1 ORDER BY id`},
	{"client_groups", `SELECT id, description, params::json AS params, allowed_user_groups::json AS allowed_user_groups
		FROM client_groups WHERE tenant_id = $1 ORDER BY id`},
	{"stored_tunnels", `SELECT id, client_id, created_at, name, scheme, remote_ip, remote_port, public_port, acl,
		further_options::json AS further_options FROM stored_tunnels WHERE tenant_id = $1 ORDER BY client_id, id`},
	{"schedules", `SELECT id, created_at, created_by, name, schedule, type, details::json AS details FROM schedules WHERE tenant_id = $1 ORDER BY id`},
	{"library", `SELECT e.id, e.name, e.description, e.tags::json AS tags, e.os::json AS os, e.active_version, e.builtin_key,
		e.created_at, e.created_by, e.updated_at, e.updated_by, v.content
		FROM library_entries e JOIN library_entry_versions v ON v.tenant_id = e.tenant_id AND v.entry_id = e.id AND v.version = e.active_version
		WHERE e.tenant_id = $1 AND e.deleted_at IS NULL ORDER BY e.name, e.id`},
}

// Export implements signup.Store.
func (s Store) Export(ctx context.Context, org string) (*signup.Export, error) {
	e := &signup.Export{Format: signup.ExportFormat, ExportedAt: time.Now().UTC()}
	sections := map[string]*[]any{
		"users": &e.Users, "user_groups": &e.UserGroups, "clients": &e.Clients, "client_groups": &e.ClientGroups,
		"stored_tunnels": &e.Tunnels, "schedules": &e.Schedules, "library": &e.Library,
	}
	err := postgres.InTenantID(ctx, s.DB, tenant.ID(org), func(tx *sqlx.Tx) error {
		var o struct {
			ID         string    `db:"id"`
			Name       string    `db:"name"`
			Status     string    `db:"status"`
			CreatedAt  time.Time `db:"created_at"`
			MaxClients *int      `db:"max_clients"`
		}
		if err := tx.GetContext(ctx, &o, "SELECT id, name, status, created_at, max_clients FROM tenants WHERE id = $1", org); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return signup.ErrNotFound
			}
			return err
		}
		e.Organization = map[string]any{"id": o.ID, "name": o.Name, "status": o.Status, "created_at": o.CreatedAt, "device_limit": o.MaxClients}
		for _, q := range exportQueries {
			var raw []byte
			if err := tx.GetContext(ctx, &raw, "SELECT coalesce(json_agg(row_to_json(t)), '[]')::text FROM ("+q.sql+") t", org); err != nil {
				return fmt.Errorf("export %s: %w", q.name, err)
			}
			var rows []any
			if err := json.Unmarshal(raw, &rows); err != nil {
				return fmt.Errorf("export %s: %w", q.name, err)
			}
			for i := range rows {
				rows[i] = scrub(rows[i])
			}
			*sections[q.name] = rows
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// secretKey reports whether a JSON key names a secret (a tunnel's auth password, a token).
func secretKey(k string) bool {
	k = strings.ToLower(k)
	for _, s := range []string{"password", "secret", "token", "private_key", "passphrase"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// scrub removes the members whose key names a secret from v, at every depth.
func scrub(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if secretKey(k) {
				delete(t, k)
				continue
			}
			t[k] = scrub(x)
		}
	case []any:
		for i := range t {
			t[i] = scrub(t[i])
		}
	}
	return v
}
