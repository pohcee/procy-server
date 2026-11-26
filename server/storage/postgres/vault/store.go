// Package vault stores the OSS vault in Postgres: each tenant's vault status and its (encrypted) values, the
// vault.DbProvider of the OSS server, scoped to the tenant of each call.
package vault

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/server/vault"
	"github.com/pohcee/procy-server/share/query"

	"github.com/pohcee/procy-server/server/storage/postgres"
)

// Store is a vault.DbProvider on Postgres.
type Store struct {
	db        *sqlx.DB
	converter *query.SQLConverter
}

var _ vault.DbProvider = (*Store)(nil)

// New returns a Store on db (migrated).
func New(db *sqlx.DB) *Store {
	return &Store{db: db, converter: query.NewSQLConverter("postgres")}
}

// Close does nothing: the database is shared and closed by its owner.
func (s *Store) Close() error { return nil }

// Factory is the vault.DbProviderFactory of a Store. The tables exist once the database is migrated, so the
// vault database needs no opening: Init does nothing and GetDbProvider always returns the Store. Whether a tenant's
// vault is set up is still its status row (passphrase vaults) or nothing at all (a managed key provider).
type Factory struct {
	store *Store
}

var _ vault.DbProviderFactory = (*Factory)(nil)

// NewFactory returns the factory of store.
func NewFactory(store *Store) *Factory { return &Factory{store: store} }

func (f *Factory) GetDbProvider() vault.DbProvider { return f.store }

func (f *Factory) Init() error { return nil }

const valueColumns = "id, client_id, coalesce(required_group, '') AS required_group, created_at, created_by, " +
	"updated_at, updated_by, key, value, type"

func (s *Store) GetStatus(ctx context.Context) (vault.DbStatus, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return vault.DbStatus{}, err
	}
	var r struct {
		ID       int    `db:"id"`
		Status   string `db:"db_status"`
		EncCheck string `db:"enc_check"`
		DecCheck []byte `db:"dec_check"`
	}
	err = postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		err := tx.GetContext(ctx, &r, `SELECT id, db_status, coalesce(enc_check, '') AS enc_check, dec_check
			FROM vault_status WHERE tenant_id = $1`, string(tid))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return vault.DbStatus{}, err
	}
	return vault.DbStatus{ID: r.ID, StatusName: r.Status, EncCheckValue: r.EncCheck, DecCheckValue: string(r.DecCheck)}, nil
}

// SetStatus stores the tenant's status. DecCheckValue is raw bytes (see Aes256PassManager.GetEncRandValue), not
// always valid UTF-8, so it is stored as bytea.
func (s *Store) SetStatus(ctx context.Context, newStatus vault.DbStatus) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	return postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO vault_status (tenant_id, db_status, enc_check, dec_check)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (tenant_id) DO UPDATE SET db_status = excluded.db_status, enc_check = excluded.enc_check,
				dec_check = excluded.dec_check`,
			string(tid), newStatus.StatusName, newStatus.EncCheckValue, []byte(newStatus.DecCheckValue))
		return err
	})
}

// getOne returns the tenant's value matching where (with its args after the tenant, $2 on).
func (s *Store) getOne(ctx context.Context, where string, args ...any) (val vault.StoredValue, found bool, err error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return val, false, err
	}
	err = postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		err := tx.GetContext(ctx, &val, "SELECT "+valueColumns+" FROM vault_values WHERE tenant_id = $1 AND "+where,
			append([]any{string(tid)}, args...)...)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return nil
	})
	return val, found, err
}

func (s *Store) GetByID(ctx context.Context, id int) (vault.StoredValue, bool, error) {
	return s.getOne(ctx, "id = $2", id)
}

func (s *Store) FindByKeyAndClientID(ctx context.Context, key, clientID string) (vault.StoredValue, bool, error) {
	return s.getOne(ctx, "key = $2 AND client_id = $3", key, clientID)
}

// filterColumns maps the list filter columns the vault API allows to what they compare in Postgres. id and
// created_at are compared as text, as SQLite compares the filter values; created_at in the RFC 3339 form SQLite
// stores (UTC, as the server writes it).
var filterColumns = map[string]string{
	"id":         "id::text",
	"created_at": `to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`,
}

func (s *Store) List(ctx context.Context, lo *query.ListOptions) ([]vault.ValueKey, error) {
	values := []vault.ValueKey{}
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return values, err
	}
	opts := query.ListOptions{}
	if lo != nil {
		opts = *lo
		opts.Filters = make([]query.FilterOption, len(lo.Filters))
		for i, f := range lo.Filters {
			cols := make([]string, len(f.Column))
			for j, c := range f.Column {
				if mapped, ok := filterColumns[c]; ok {
					c = mapped
				}
				cols[j] = c
			}
			f.Column = cols
			opts.Filters[i] = f
		}
	}
	if len(opts.Sorts) == 0 {
		opts.Sorts = []query.SortOption{{Column: "id", IsASC: true}}
	}
	q, params := s.converter.AppendOptionsToQuery(&opts,
		"SELECT id, client_id, created_by, created_at, key FROM vault_values WHERE tenant_id = ?", []any{string(tid)})
	err = postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &values, tx.Rebind(q), params...)
	})
	return values, err
}

func (s *Store) Save(ctx context.Context, user string, idToUpdate int64, val *vault.InputValue, nowDate time.Time) (int64, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return 0, err
	}
	// SQLite stores the time as RFC 3339, to the second
	now := nowDate.Truncate(time.Second)
	err = postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		if idToUpdate == 0 {
			return tx.GetContext(ctx, &idToUpdate, `INSERT INTO vault_values
				(tenant_id, client_id, required_group, created_at, created_by, updated_at, updated_by, key, value, type)
				VALUES ($1, $2, $3, $4, $5, $4, $5, $6, $7, $8) RETURNING id`,
				string(tid), val.ClientID, val.RequiredGroup, now, user, val.Key, val.Value, string(val.Type))
		}
		_, err := tx.ExecContext(ctx, `UPDATE vault_values SET client_id = $1, required_group = $2, updated_at = $3,
				updated_by = $4, key = $5, value = $6, type = $7
			WHERE tenant_id = $8 AND id = $9`,
			val.ClientID, val.RequiredGroup, now, user, val.Key, val.Value, string(val.Type), string(tid), idToUpdate)
		return err
	})
	if err != nil {
		return 0, err
	}
	return idToUpdate, nil
}

func (s *Store) Delete(ctx context.Context, id int) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	return postgres.InTenantID(ctx, s.db, tid, func(tx *sqlx.Tx) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM vault_values WHERE tenant_id = $1 AND id = $2", string(tid), id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("cannot find entry by id %d", id)
		}
		return nil
	})
}
