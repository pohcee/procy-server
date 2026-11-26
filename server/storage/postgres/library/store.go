// Package library is the Postgres library.Repository: library entries and their version history, per tenant.
package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/server/library"
	"github.com/pohcee/procy-server/share/tenant"

	"github.com/pohcee/procy-server/server/storage/postgres"
)

// Store keeps the library in the tables library_entries, library_entry_versions and library_meta.
type Store struct {
	db *sqlx.DB
}

var _ library.Repository = (*Store)(nil)

// New returns the library store on db (shared; Close leaves it open).
func New(db *sqlx.DB) *Store {
	return &Store{db: db}
}

// inTenant runs fn in a transaction of ctx's tenant and passes it the tenant.
func (s *Store) inTenant(ctx context.Context, fn func(tx *sqlx.Tx, tid tenant.ID) error) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	return postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error { return fn(tx, tid) })
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func toJSON(v interface{}) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func insertVersion(ctx context.Context, tx *sqlx.Tx, tid tenant.ID, v library.Version) error {
	changed, err := toJSON(nonNilStrings(v.Changed))
	if err != nil {
		return err
	}
	content, err := toJSON(v.Content)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO library_entry_versions (tenant_id, entry_id, version, created_at, created_by, message, changed, content)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		tid, v.EntryID, v.Version, v.CreatedAt.UTC(), v.CreatedBy, v.Message, changed, content)
	return err
}

// Insert stores a new entry together with its first version. ErrNameTaken when a live entry has the name.
func (s *Store) Insert(ctx context.Context, e library.Entry, v library.Version) error {
	return s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		tags, err := toJSON(nonNilStrings(e.Tags))
		if err != nil {
			return err
		}
		osKeys, err := toJSON(nonNilStrings(e.OS))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO library_entries (tenant_id, id, name, description, tags, os, active_version, builtin_key, created_at, created_by, updated_at, updated_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			tid, e.ID, e.Name, e.Description, tags, osKeys, e.ActiveVersion, e.BuiltinKey, e.CreatedAt.UTC(), e.CreatedBy, e.UpdatedAt.UTC(), e.UpdatedBy)
		if isUniqueViolation(err) {
			return library.ErrNameTaken
		}
		if err != nil {
			return err
		}
		return insertVersion(ctx, tx, tid, v)
	})
}

// AddVersion makes v the active version of e, provided the entry is live and its active version is still
// baseVersion (ErrConflict otherwise).
func (s *Store) AddVersion(ctx context.Context, e library.Entry, v library.Version, baseVersion int) error {
	return s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		tags, err := toJSON(nonNilStrings(e.Tags))
		if err != nil {
			return err
		}
		osKeys, err := toJSON(nonNilStrings(e.OS))
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE library_entries SET name = $1, description = $2, tags = $3, os = $4, updated_at = $5, updated_by = $6, active_version = $7
			 WHERE tenant_id = $8 AND id = $9 AND active_version = $10 AND deleted_at IS NULL`,
			e.Name, e.Description, tags, osKeys, e.UpdatedAt.UTC(), e.UpdatedBy, v.Version, tid, e.ID, baseVersion)
		if isUniqueViolation(err) {
			return library.ErrNameTaken
		}
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return library.ErrConflict
		}
		return insertVersion(ctx, tx, tid, v)
	})
}

type entryRow struct {
	ID            string     `db:"id"`
	Name          string     `db:"name"`
	Description   string     `db:"description"`
	Tags          string     `db:"tags"`
	OS            string     `db:"os"`
	ActiveVersion int        `db:"active_version"`
	BuiltinKey    *string    `db:"builtin_key"`
	CreatedAt     time.Time  `db:"created_at"`
	CreatedBy     string     `db:"created_by"`
	UpdatedAt     time.Time  `db:"updated_at"`
	UpdatedBy     string     `db:"updated_by"`
	DeletedAt     *time.Time `db:"deleted_at"`
	DeletedBy     string     `db:"deleted_by"`
}

const entryColumns = "id, name, description, tags, os, active_version, builtin_key, created_at, created_by, updated_at, updated_by, deleted_at, deleted_by"

// entryOrder is SQLite's ORDER BY name COLLATE NOCASE, id: case-insensitive, otherwise byte order.
const entryOrder = ` ORDER BY lower(name) COLLATE "C", id`

func (r entryRow) entry() (library.Entry, error) {
	e := library.Entry{
		ID: r.ID, Name: r.Name, Description: r.Description, ActiveVersion: r.ActiveVersion,
		BuiltinKey: r.BuiltinKey, CreatedAt: r.CreatedAt.UTC(), CreatedBy: r.CreatedBy,
		UpdatedAt: r.UpdatedAt.UTC(), UpdatedBy: r.UpdatedBy, DeletedAt: utcPtr(r.DeletedAt), DeletedBy: r.DeletedBy,
	}
	if err := json.Unmarshal([]byte(r.Tags), &e.Tags); err != nil {
		return e, err
	}
	if err := json.Unmarshal([]byte(r.OS), &e.OS); err != nil {
		return e, err
	}
	return e, nil
}

// Get returns the entry with its active version's implementations, or nil if absent.
func (s *Store) Get(ctx context.Context, id string) (*library.Entry, error) {
	var out *library.Entry
	err := s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		e, err := getEntry(ctx, tx, tid, "id", id)
		out = e
		return err
	})
	return out, err
}

// getEntry returns the entry whose column col (id or builtin_key) is val, with its active version's
// implementations, or nil.
func getEntry(ctx context.Context, tx *sqlx.Tx, tid tenant.ID, col, val string) (*library.Entry, error) {
	var row struct {
		entryRow
		Content *string `db:"content"`
	}
	err := tx.GetContext(ctx, &row,
		"SELECT e."+strings.ReplaceAll(entryColumns, ", ", ", e.")+", v.content AS content FROM library_entries e"+
			" LEFT JOIN library_entry_versions v ON v.tenant_id = e.tenant_id AND v.entry_id = e.id AND v.version = e.active_version"+
			" WHERE e.tenant_id = $1 AND e."+col+" = $2 ORDER BY e.id LIMIT 1", tid, val)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e, err := row.entry()
	if err != nil {
		return nil, err
	}
	if row.Content != nil {
		var c library.Content
		if err := json.Unmarshal([]byte(*row.Content), &c); err != nil {
			return nil, err
		}
		e.Implementations = c.Implementations
	}
	return &e, nil
}

// List returns entries without implementations, ordered by name.
func (s *Store) List(ctx context.Context, includeDeleted bool) ([]library.Entry, error) {
	var entries []library.Entry
	err := s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		q := "SELECT " + entryColumns + " FROM library_entries WHERE tenant_id = $1"
		if !includeDeleted {
			q += " AND deleted_at IS NULL"
		}
		var rows []entryRow
		if err := tx.SelectContext(ctx, &rows, q+entryOrder, tid); err != nil {
			return err
		}
		entries = make([]library.Entry, 0, len(rows))
		for _, r := range rows {
			e, err := r.entry()
			if err != nil {
				return err
			}
			entries = append(entries, e)
		}
		return nil
	})
	return entries, err
}

// ListLive returns the live entries with their active version's implementations, ordered by name.
func (s *Store) ListLive(ctx context.Context) ([]library.Entry, error) {
	var entries []library.Entry
	err := s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		var rows []struct {
			entryRow
			Content string `db:"content"`
		}
		q := "SELECT e." + strings.ReplaceAll(entryColumns, ", ", ", e.") + ", v.content AS content FROM library_entries e" +
			" JOIN library_entry_versions v ON v.tenant_id = e.tenant_id AND v.entry_id = e.id AND v.version = e.active_version" +
			" WHERE e.tenant_id = $1 AND e.deleted_at IS NULL ORDER BY lower(e.name) COLLATE \"C\", e.id"
		if err := tx.SelectContext(ctx, &rows, q, tid); err != nil {
			return err
		}
		entries = make([]library.Entry, 0, len(rows))
		for _, r := range rows {
			e, err := r.entry()
			if err != nil {
				return err
			}
			var c library.Content
			if err := json.Unmarshal([]byte(r.Content), &c); err != nil {
				return err
			}
			e.Implementations = c.Implementations
			entries = append(entries, e)
		}
		return nil
	})
	return entries, err
}

type versionRow struct {
	EntryID   string    `db:"entry_id"`
	Version   int       `db:"version"`
	CreatedAt time.Time `db:"created_at"`
	CreatedBy string    `db:"created_by"`
	Message   string    `db:"message"`
	Changed   string    `db:"changed"`
	Content   string    `db:"content"`
}

func (r versionRow) version(withContent bool) (library.Version, error) {
	v := library.Version{EntryID: r.EntryID, Version: r.Version, CreatedAt: r.CreatedAt.UTC(), CreatedBy: r.CreatedBy, Message: r.Message}
	if err := json.Unmarshal([]byte(r.Changed), &v.Changed); err != nil {
		return v, err
	}
	if withContent {
		v.Content = &library.Content{}
		if err := json.Unmarshal([]byte(r.Content), v.Content); err != nil {
			return v, err
		}
	}
	return v, nil
}

// Versions lists an entry's versions, newest first, without content.
func (s *Store) Versions(ctx context.Context, id string) ([]library.Version, error) {
	var versions []library.Version
	err := s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		var rows []versionRow
		if err := tx.SelectContext(ctx, &rows,
			`SELECT entry_id, version, created_at, created_by, message, changed, '' AS content FROM library_entry_versions
			 WHERE tenant_id = $1 AND entry_id = $2 ORDER BY version DESC`, tid, id); err != nil {
			return err
		}
		versions = make([]library.Version, 0, len(rows))
		for _, r := range rows {
			v, err := r.version(false)
			if err != nil {
				return err
			}
			versions = append(versions, v)
		}
		return nil
	})
	return versions, err
}

// Version returns one version with its content, or nil if absent.
func (s *Store) Version(ctx context.Context, id string, n int) (*library.Version, error) {
	var out *library.Version
	err := s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		var row versionRow
		err := tx.GetContext(ctx, &row,
			`SELECT entry_id, version, created_at, created_by, message, changed, content FROM library_entry_versions
			 WHERE tenant_id = $1 AND entry_id = $2 AND version = $3`, tid, id, n)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		v, err := row.version(true)
		if err != nil {
			return err
		}
		out = &v
		return nil
	})
	return out, err
}

// SetDeleted marks an entry deleted at the given time, or undeletes it when at is nil (ErrNameTaken when a live
// entry has its name by then).
func (s *Store) SetDeleted(ctx context.Context, id string, at *time.Time, by string) error {
	return s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		if at == nil {
			by = ""
		}
		_, err := tx.ExecContext(ctx, "UPDATE library_entries SET deleted_at = $1, deleted_by = $2 WHERE tenant_id = $3 AND id = $4",
			utcPtr(at), by, tid, id)
		if isUniqueViolation(err) {
			return library.ErrNameTaken
		}
		return err
	})
}

// Prune deletes old versions and returns how many were removed. A version goes when it is not among the newest
// `keep` (if keep > 0) or is older than olderThan (if non-zero). The active version and version 1 stay.
func (s *Store) Prune(ctx context.Context, keep int, olderThan time.Time) (int64, error) {
	var removed int64
	err := s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		if keep <= 0 && olderThan.IsZero() {
			return nil
		}
		var conds []string
		args := []interface{}{tid}
		if keep > 0 {
			args = append(args, keep)
			conds = append(conds, "(SELECT count(*) FROM library_entry_versions n WHERE n.tenant_id = v.tenant_id AND n.entry_id = v.entry_id AND n.version > v.version) >= $2")
		}
		if !olderThan.IsZero() {
			args = append(args, olderThan.UTC())
			conds = append(conds, "v.created_at < $"+strconv.Itoa(len(args)))
		}
		res, err := tx.ExecContext(ctx,
			"DELETE FROM library_entry_versions v WHERE v.tenant_id = $1 AND v.version > 1"+
				" AND v.version <> (SELECT e.active_version FROM library_entries e WHERE e.tenant_id = v.tenant_id AND e.id = v.entry_id)"+
				" AND ("+strings.Join(conds, " OR ")+")", args...)
		if err != nil {
			return err
		}
		removed, err = res.RowsAffected()
		return err
	})
	return removed, err
}

// Meta reads a library metadata value.
func (s *Store) Meta(ctx context.Context, key string) (string, bool, error) {
	var value string
	var found bool
	err := s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		err := tx.GetContext(ctx, &value, "SELECT value FROM library_meta WHERE tenant_id = $1 AND key = $2", tid, key)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return nil
	})
	return value, found, err
}

// ByBuiltinKey returns the entry (deleted or not) created for a built-in key, or nil.
func (s *Store) ByBuiltinKey(ctx context.Context, key string) (*library.Entry, error) {
	var out *library.Entry
	err := s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		e, err := getEntry(ctx, tx, tid, "builtin_key", key)
		out = e
		return err
	})
	return out, err
}

// Close does nothing: the database is shared and closed by its owner.
func (s *Store) Close() error { return nil }
