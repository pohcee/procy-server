package postgres

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/share/tenant"
)

// TenantSetting is the transaction-local setting the row-level security policies compare tenant_id with.
const TenantSetting = "rport.tenant_id"

// TenantOf returns the tenant carried by ctx, or an error wrapping tenant.ErrMissing.
func TenantOf(ctx context.Context) (tenant.ID, error) {
	id, err := tenant.FromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("pg: %w", err)
	}
	return id, nil
}

// InTenant runs fn in a transaction scoped to the tenant carried by ctx: the transaction-local rport.tenant_id is
// set, so row-level security shows fn only that tenant's rows. The transaction commits when fn returns nil and rolls
// back otherwise. Without a tenant in ctx it returns an error wrapping tenant.ErrMissing and never touches db.
func InTenant(ctx context.Context, db *sqlx.DB, fn func(tx *sqlx.Tx) error) error {
	id, err := TenantOf(ctx)
	if err != nil {
		return err
	}
	return InTenantID(ctx, db, id, fn)
}

// InTenantID is InTenant for an explicit tenant, for work that has no tenant in its context yet (for example
// creating a tenant's first rows).
func InTenantID(ctx context.Context, db *sqlx.DB, id tenant.ID, fn func(tx *sqlx.Tx) error) (err error) {
	if id == "" {
		return fmt.Errorf("pg: %w", tenant.ErrMissing)
	}
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pg: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback() // don't leave the connection in an open transaction
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, "SELECT set_config($1, $2, true)", TenantSetting, string(id)); err != nil {
		return fmt.Errorf("pg: set tenant: %w", err)
	}
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("pg: commit: %w", err)
	}
	return nil
}
