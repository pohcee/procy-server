// Package clientauth stores client authentication credentials in Postgres: the clientsauth.Provider of the OSS
// server, scoped to the tenant of each call. Client ids are unique across tenants (a connecting client doesn't name
// its tenant), so adding an id another tenant uses reports it as existing.
package clientauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/clientsauth"
	"github.com/openrport/openrport/share/enums"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"

	"github.com/openrport/openrport/server/storage/postgres"
)

// Store is a writeable clientsauth.Provider on Postgres.
type Store struct {
	db        *sqlx.DB
	converter *query.SQLConverter
}

var _ clientsauth.Provider = (*Store)(nil)

// New returns a Store on db (migrated).
func New(db *sqlx.DB) *Store {
	return &Store{db: db, converter: query.NewSQLConverter("postgres")}
}

// Close does nothing: the database is shared and closed by its owner.
func (s *Store) Close() error { return nil }

// IsWriteable is true: credentials can be added and deleted through the API.
func (s *Store) IsWriteable() bool { return true }

// Source reports a database provider.
func (s *Store) Source() enums.ProviderSource { return enums.ProviderSourceDB }

const columns = "id, coalesce(password, '') AS password"

// TenantOf returns the tenant holding the credentials with the client auth id, and false if no tenant has them. It
// needs no tenant in ctx: it reads the global client_auth_ids table (kept by a trigger on clients_auth), which has no
// row-level security. The credentials themselves are read with Get in that tenant.
func (s *Store) TenantOf(ctx context.Context, id string) (tenant.ID, bool, error) {
	var tid string
	err := s.db.GetContext(ctx, &tid, "SELECT tenant_id FROM client_auth_ids WHERE id = $1", id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("client auth: tenant of %q: %w", id, err)
	}
	return tenant.ID(tid), true, nil
}

func (s *Store) Get(ctx context.Context, id string) (*clientsauth.ClientAuth, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var out *clientsauth.ClientAuth
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		c := &clientsauth.ClientAuth{}
		err := tx.GetContext(ctx, c, "SELECT "+columns+" FROM clients_auth WHERE tenant_id = $1 AND id = $2", string(tid), id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = c
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("client auth: get %q: %w", id, err)
	}
	return out, nil
}

// GetFiltered returns one page of the credentials matching filter.Filters, sorted by id (bytewise, as SQLite), and
// the number of all matching ones. Only the filters of clientsauth.SupportedFilters are accepted; sorts are ignored
// (the API offers none). filter is not modified.
func (s *Store) GetFiltered(ctx context.Context, filter *query.ListOptions) ([]*clientsauth.ClientAuth, int, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return nil, 0, err
	}
	var filters []query.FilterOption
	var pagination *query.Pagination
	if filter != nil {
		filters, pagination = filter.Filters, filter.Pagination
	}
	for _, f := range filters {
		for _, col := range f.Column {
			if !clientsauth.SupportedFilters[col] {
				return nil, 0, fmt.Errorf("client auth: unsupported filter %q", col)
			}
		}
	}
	where, params := s.converter.AddWhere(filters, "FROM clients_auth WHERE tenant_id = ?", []any{string(tid)})

	list := []*clientsauth.ClientAuth{}
	count := 0
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		if err := tx.GetContext(ctx, &count, tx.Rebind("SELECT count(*) "+where), params...); err != nil {
			return err
		}
		q := "SELECT " + columns + " " + where + ` ORDER BY id COLLATE "C"`
		args := params
		if pagination != nil {
			limit, offset, err := pageOf(pagination)
			if err != nil {
				return err
			}
			q += " LIMIT ? OFFSET ?"
			args = append(append([]any{}, params...), limit, offset)
		}
		return tx.SelectContext(ctx, &list, tx.Rebind(q), args...)
	})
	if err != nil {
		return nil, 0, fmt.Errorf("client auth: list: %w", err)
	}
	return list, count, nil
}

// pageOf returns the limit and offset of p as numbers (the API validates them; Limit and Offset are their text).
func pageOf(p *query.Pagination) (limit, offset int, err error) {
	if limit, err = strconv.Atoi(p.Limit); err != nil {
		return 0, 0, fmt.Errorf("pagination limit %q: %w", p.Limit, err)
	}
	if p.Offset == "" {
		return limit, 0, nil
	}
	if offset, err = strconv.Atoi(p.Offset); err != nil {
		return 0, 0, fmt.Errorf("pagination offset %q: %w", p.Offset, err)
	}
	return limit, offset, nil
}

// Add stores the credentials and returns true, or false if the id exists already, in this tenant or another one.
func (s *Store) Add(ctx context.Context, client *clientsauth.ClientAuth) (bool, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return false, err
	}
	added := false
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		// no conflict target: the primary key and the global UNIQUE (id) both count as "exists"
		res, err := tx.ExecContext(ctx, "INSERT INTO clients_auth (tenant_id, id, password) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING",
			string(tid), client.ID, client.Password)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		added = n == 1
		return err
	})
	if err != nil {
		return false, fmt.Errorf("client auth: add %q: %w", client.ID, err)
	}
	return added, nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	err = postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM clients_auth WHERE tenant_id = $1 AND id = $2", string(tid), id)
		return err
	})
	if err != nil {
		return fmt.Errorf("client auth: delete %q: %w", id, err)
	}
	return nil
}
