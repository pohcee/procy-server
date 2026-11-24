package pgserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/library"
	pglibrary "github.com/openrport/openrport/server/storage/postgres/library"
	pgusers "github.com/openrport/openrport/server/storage/postgres/users"
	chshare "github.com/openrport/openrport/share"
	"github.com/openrport/openrport/share/tenant"
)

// AddTenant creates a tenant and gives it the built-in library entries. db must be allowed to write the tenants
// table (the schema owner or the application role). Reserved ids ("default", ids starting with "_") are refused.
func AddTenant(ctx context.Context, db *sqlx.DB, id tenant.ID, name string) error {
	if id == tenant.Default || strings.HasPrefix(string(id), "_") {
		return fmt.Errorf("tenant id %q is reserved", id)
	}
	if name == "" {
		name = string(id)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO tenants (id, name) VALUES ($1, $2)", string(id), name); err != nil {
		return fmt.Errorf("add tenant %q: %w", id, err)
	}
	builtins, err := library.Builtins()
	if err != nil {
		return fmt.Errorf("tenant %q added, but the built-in library entries could not be loaded: %w", id, err)
	}
	tctx := tenant.WithID(ctx, id)
	if _, _, _, err := library.SyncBuiltins(tctx, pglibrary.New(db), builtins, chshare.BuildVersion, time.Now()); err != nil {
		return fmt.Errorf("tenant %q added, but the built-in library entries could not be synced: %w", id, err)
	}
	return nil
}

// AddUser creates a user in a tenant through the server's own user service (validation, password hashing). groups
// defaults to Administrators.
func AddUser(ctx context.Context, db *sqlx.DB, id tenant.ID, username, password string, groups []string) error {
	if len(groups) == 0 {
		groups = []string{users.Administrators}
	}
	var status string
	if err := db.GetContext(ctx, &status, "SELECT status FROM tenants WHERE id = $1", string(id)); err != nil {
		return fmt.Errorf("tenant %q: %w", id, err)
	}
	if status != "active" {
		return fmt.Errorf("tenant %q is %s; users can be added only to active tenants", id, status)
	}
	svc := users.NewAPIService(pgusers.New(db), false, 14, 0)
	err := svc.Change(tenant.WithID(ctx, id), &users.User{Username: username, Password: password, Groups: groups}, "")
	if err != nil {
		return fmt.Errorf("add user %q: %w", username, err)
	}
	return nil
}
