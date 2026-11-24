// Package postgres is the Postgres storage of the server (one database for many tenants): opening the database, running every repository call inside a
// tenant transaction, and the row-level security that backs the repositories' own tenant filtering.
//
// Every tenant-scoped table has a tenant_id column, keys that start with it, and the tenant_isolation policy
// (created by the SQL function rport_enable_rls in migration 0001): a row is visible and writable only while the
// transaction-local setting rport.tenant_id names its tenant. InTenant sets it. Repository queries still filter by
// tenant_id themselves; row-level security is the second layer. It applies to the application role, which must not
// own the tables nor be a superuser (see migrate.GrantApp).
package postgres

import (
	"context"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/jmoiron/sqlx"
)

// DriverName is the database/sql driver the Postgres storage uses. sqlx binds "$n" placeholders for it.
const DriverName = "pgx"

// Open connects to the database at dsn (a postgres:// URL or key=value string) and checks the connection.
func Open(ctx context.Context, dsn string) (*sqlx.DB, error) {
	db, err := sqlx.Open(DriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("pg: open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pg: connect: %w", err)
	}
	return db, nil
}
