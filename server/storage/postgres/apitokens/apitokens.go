// Package apitokens stores API tokens in Postgres: the authorization.DbProvider of the OSS API, scoped to the
// tenant of each call.
package apitokens

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/server/api/authorization"

	"github.com/pohcee/procy-server/server/storage/postgres"
)

// Store is an authorization.DbProvider on Postgres.
type Store struct {
	db *sqlx.DB
}

var _ authorization.DbProvider = (*Store)(nil)

// New returns a Store on db (migrated).
func New(db *sqlx.DB) *Store { return &Store{db: db} }

// Close does nothing: the database is shared and closed by its owner.
func (s *Store) Close() error { return nil }

const columns = "tenant_id, username, prefix, name, created_at, expires_at, coalesce(scope, '') AS scope, token"

func (s *Store) GetAll(ctx context.Context, username string) ([]*authorization.APIToken, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out []*authorization.APIToken
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &out, "SELECT "+columns+" FROM api_tokens WHERE tenant_id = $1 AND username = $2",
			string(tid), username)
	})
	if err != nil {
		return nil, fmt.Errorf("unable to get api_token from DB: %w", err)
	}
	return out, nil
}

func (s *Store) Get(ctx context.Context, username, prefix string) (*authorization.APIToken, error) {
	return s.getOne(ctx, "prefix", username, prefix)
}

func (s *Store) GetByName(ctx context.Context, username, name string) (*authorization.APIToken, error) {
	return s.getOne(ctx, "name", username, name)
}

// getOne returns the token of username whose column (prefix or name) is value, or nil.
func (s *Store) getOne(ctx context.Context, column, username, value string) (*authorization.APIToken, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *authorization.APIToken
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		tk := &authorization.APIToken{}
		err := tx.GetContext(ctx, tk, "SELECT "+columns+" FROM api_tokens WHERE tenant_id = $1 AND username = $2 AND "+column+" = $3",
			string(tid), username, value)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = tk
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("unable to get api token from DB: %w", err)
	}
	return out, nil
}

// Save inserts the token or, if the user already has one with the prefix, updates its name (when given) and its
// expiry (when given); the token, scope and creation time of an existing token never change.
func (s *Store) Save(ctx context.Context, tokenLine *authorization.APIToken) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	tokenLine.TenantID = tid
	return postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO api_tokens (tenant_id, username, prefix, name, created_at, expires_at, scope, token)
			VALUES ($1, $2, $3, $4, coalesce($5::timestamptz, now()), $6, $7, $8)
			ON CONFLICT (tenant_id, username, prefix) DO UPDATE SET
				expires_at = coalesce(EXCLUDED.expires_at, api_tokens.expires_at),
				name = CASE WHEN EXCLUDED.name <> '' THEN EXCLUDED.name ELSE api_tokens.name END`,
			string(tid), tokenLine.Username, tokenLine.Prefix, tokenLine.Name, tokenLine.CreatedAt, tokenLine.ExpiresAt,
			string(tokenLine.Scope), tokenLine.Token)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("cannot Insert or Update token %s", tokenLine.Prefix)
		}
		return nil
	})
}

func (s *Store) Delete(ctx context.Context, username, prefix string) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	return postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM api_tokens WHERE tenant_id = $1 AND username = $2 AND prefix = $3",
			string(tid), username, prefix)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("cannot find API Token by prefix %s", prefix)
		}
		return nil
	})
}
