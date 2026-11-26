// Package vaultkeys stores the wrapped data encryption key of each tenant's vault in Postgres: the
// vaultkms.WrappedKeyStore every server instance shares.
package vaultkeys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/share/tenant"

	"github.com/pohcee/procy-server/server/storage/postgres"
	"github.com/pohcee/procy-server/server/vault/vaultkms"
)

// Store is a vaultkms.WrappedKeyStore on Postgres. Its methods take the tenant explicitly (not from the context)
// and refuse an empty one with tenant.ErrMissing.
type Store struct {
	db *sqlx.DB
}

var _ vaultkms.WrappedKeyStore = (*Store)(nil)

// New returns a Store on db (migrated).
func New(db *sqlx.DB) *Store { return &Store{db: db} }

type row struct {
	KeyName    string `db:"key_name"`
	Ciphertext []byte `db:"ciphertext"`
	Shredded   bool   `db:"shredded"`
}

func (r row) wrapped() vaultkms.WrappedKey {
	return vaultkms.WrappedKey{KeyName: r.KeyName, Ciphertext: r.Ciphertext, Shredded: r.Shredded}
}

func checkTenant(tid tenant.ID) error {
	if tid == "" {
		return fmt.Errorf("vaultkeys: %w", tenant.ErrMissing)
	}
	return nil
}

func get(ctx context.Context, tx *sqlx.Tx, tid tenant.ID) (vaultkms.WrappedKey, bool, error) {
	var r row
	err := tx.GetContext(ctx, &r, "SELECT key_name, ciphertext, shredded FROM vault_wrapped_keys WHERE tenant_id = $1", string(tid))
	if errors.Is(err, sql.ErrNoRows) {
		return vaultkms.WrappedKey{}, false, nil
	}
	if err != nil {
		return vaultkms.WrappedKey{}, false, err
	}
	return r.wrapped(), true, nil
}

func (s *Store) Get(ctx context.Context, tid tenant.ID) (key vaultkms.WrappedKey, found bool, err error) {
	if err := checkTenant(tid); err != nil {
		return key, false, err
	}
	err = postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		key, found, err = get(ctx, tx, tid)
		return err
	})
	if err != nil {
		return vaultkms.WrappedKey{}, false, fmt.Errorf("vaultkeys: get: %w", err)
	}
	return key, found, nil
}

// CreateIfAbsent inserts key unless the tenant has a row (a key or a tombstone) and returns the row stored. A
// concurrent insert of another key waits on the primary key and then does nothing, so both callers read the winner's.
func (s *Store) CreateIfAbsent(ctx context.Context, tid tenant.ID, key vaultkms.WrappedKey) (stored vaultkms.WrappedKey, err error) {
	if err := checkTenant(tid); err != nil {
		return stored, err
	}
	err = postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO vault_wrapped_keys (tenant_id, key_name, ciphertext, shredded)
			VALUES ($1, $2, $3, $4) ON CONFLICT (tenant_id) DO NOTHING`,
			string(tid), key.KeyName, key.Ciphertext, key.Shredded)
		if err != nil {
			return err
		}
		var found bool
		stored, found, err = get(ctx, tx, tid)
		if err == nil && !found {
			err = errors.New("no key stored after insert")
		}
		return err
	})
	if err != nil {
		return vaultkms.WrappedKey{}, fmt.Errorf("vaultkeys: create: %w", err)
	}
	return stored, nil
}

func (s *Store) Shred(ctx context.Context, tid tenant.ID) error {
	if err := checkTenant(tid); err != nil {
		return err
	}
	err := postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO vault_wrapped_keys (tenant_id, key_name, ciphertext, shredded)
			VALUES ($1, '', NULL, true)
			ON CONFLICT (tenant_id) DO UPDATE SET key_name = '', ciphertext = NULL, shredded = true, updated_at = now()`,
			string(tid))
		return err
	})
	if err != nil {
		return fmt.Errorf("vaultkeys: shred: %w", err)
	}
	return nil
}
