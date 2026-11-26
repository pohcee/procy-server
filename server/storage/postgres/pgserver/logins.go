package pgserver

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/server/storage/postgres"
	"github.com/pohcee/procy-server/share/tenant"
)

// LoginRecorder records full sign-ins (users.last_login_at and accounts.last_login_at, migration 1750).
type LoginRecorder struct {
	db *sqlx.DB
}

// NewLoginRecorder returns the recorder on db.
func NewLoginRecorder(db *sqlx.DB) *LoginRecorder { return &LoginRecorder{db: db} }

// RecordLogin records that username signed in to the tenant of ctx now.
func (l *LoginRecorder) RecordLogin(ctx context.Context, username string) error {
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		return err
	}
	err = postgres.InTenantID(ctx, l.db, tid, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, "SELECT rport_record_login($1)", username)
		return err
	})
	if err != nil {
		return fmt.Errorf("record the sign-in of %q: %w", username, err)
	}
	return nil
}
