package postgres

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// CheckAppRole returns an error when the role db connects as would not be subject to row-level security: a
// superuser, a role with BYPASSRLS, or the owner of any table, or a member of a role that owns one (the schema owner can
// disable or drop the policies, and so can any role holding its rights). A server must
// connect as a separate application role (see migrate.GrantApp).
func CheckAppRole(ctx context.Context, db *sqlx.DB) error {
	var r struct {
		Name      string `db:"name"`
		Super     bool   `db:"super"`
		BypassRLS bool   `db:"bypass"`
		Owned     int    `db:"owned"`
	}
	err := db.GetContext(ctx, &r, `SELECT current_user AS name, r.rolsuper AS super, r.rolbypassrls AS bypass,
		(SELECT count(*) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema')
			AND pg_has_role(current_user, tableowner, 'MEMBER')) AS owned
		FROM pg_roles r WHERE r.rolname = current_user`)
	if err != nil {
		return fmt.Errorf("pg: check role: %w", err)
	}
	switch {
	case r.Super, r.BypassRLS:
		return fmt.Errorf("pg: role %q bypasses row-level security (superuser or BYPASSRLS); connect as the application role", r.Name)
	case r.Owned > 0:
		return fmt.Errorf("pg: role %q owns %d table(s), or has the rights of their owner, and could turn row-level security off; connect as the application role", r.Name, r.Owned)
	}
	return nil
}
