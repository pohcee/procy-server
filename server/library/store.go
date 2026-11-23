package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

var (
	ErrConflict  = errors.New("library entry was changed by someone else")
	ErrNameTaken = errors.New("a library entry with this name already exists")
)

// Store persists entries and their versions in library.db.
type Store struct {
	db *sqlx.DB
}

func NewStore(db *sqlx.DB) *Store {
	return &Store{db: db}
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
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

func insertVersion(ctx context.Context, tx *sqlx.Tx, v Version) error {
	changed, err := toJSON(nonNilStrings(v.Changed))
	if err != nil {
		return err
	}
	content, err := toJSON(v.Content)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		"INSERT INTO `entry_versions` (`entry_id`, `version`, `created_at`, `created_by`, `message`, `changed`, `content`) VALUES (?, ?, ?, ?, ?, ?, ?)",
		v.EntryID, v.Version, v.CreatedAt.UTC(), v.CreatedBy, v.Message, changed, content)
	return err
}

// Insert stores a new entry together with its first version.
func (s *Store) Insert(ctx context.Context, e Entry, v Version) error {
	tags, err := toJSON(nonNilStrings(e.Tags))
	if err != nil {
		return err
	}
	osKeys, err := toJSON(nonNilStrings(e.OS))
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	_, err = tx.ExecContext(ctx,
		"INSERT INTO `entries` (`id`, `name`, `description`, `tags`, `os`, `active_version`, `builtin_key`, `created_at`, `created_by`, `updated_at`, `updated_by`)"+
			" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		e.ID, e.Name, e.Description, tags, osKeys, e.ActiveVersion, e.BuiltinKey, e.CreatedAt.UTC(), e.CreatedBy, e.UpdatedAt.UTC(), e.UpdatedBy)
	if isUniqueViolation(err) {
		return ErrNameTaken
	}
	if err != nil {
		return err
	}
	if err = insertVersion(ctx, tx, v); err != nil {
		return err
	}
	return tx.Commit()
}

// AddVersion makes v the active version of e, provided the entry is live and its
// active version is still baseVersion.
func (s *Store) AddVersion(ctx context.Context, e Entry, v Version, baseVersion int) error {
	tags, err := toJSON(nonNilStrings(e.Tags))
	if err != nil {
		return err
	}
	osKeys, err := toJSON(nonNilStrings(e.OS))
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.ExecContext(ctx,
		"UPDATE `entries` SET `name` = ?, `description` = ?, `tags` = ?, `os` = ?, `updated_at` = ?, `updated_by` = ?, `active_version` = ?"+
			" WHERE `id` = ? AND `active_version` = ? AND `deleted_at` IS NULL",
		e.Name, e.Description, tags, osKeys, e.UpdatedAt.UTC(), e.UpdatedBy, v.Version, e.ID, baseVersion)
	if isUniqueViolation(err) {
		return ErrNameTaken
	}
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	if err = insertVersion(ctx, tx, v); err != nil {
		return err
	}
	return tx.Commit()
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

const entryColumns = "`id`, `name`, `description`, `tags`, `os`, `active_version`, `builtin_key`, `created_at`, `created_by`, `updated_at`, `updated_by`, `deleted_at`, `deleted_by`"

func (r entryRow) entry() (Entry, error) {
	e := Entry{
		ID: r.ID, Name: r.Name, Description: r.Description, ActiveVersion: r.ActiveVersion,
		BuiltinKey: r.BuiltinKey, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy,
		UpdatedAt: r.UpdatedAt, UpdatedBy: r.UpdatedBy, DeletedAt: r.DeletedAt, DeletedBy: r.DeletedBy,
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
func (s *Store) Get(ctx context.Context, id string) (*Entry, error) {
	var row entryRow
	err := s.db.GetContext(ctx, &row, "SELECT "+entryColumns+" FROM `entries` WHERE `id` = ?", id)
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
	v, err := s.Version(ctx, id, e.ActiveVersion)
	if err != nil {
		return nil, err
	}
	if v != nil && v.Content != nil {
		e.Implementations = v.Content.Implementations
	}
	return &e, nil
}

// List returns entries without implementations, ordered by name.
func (s *Store) List(ctx context.Context, includeDeleted bool) ([]Entry, error) {
	q := "SELECT " + entryColumns + " FROM `entries`"
	if !includeDeleted {
		q += " WHERE `deleted_at` IS NULL"
	}
	q += " ORDER BY `name` COLLATE NOCASE, `id`"
	var rows []entryRow
	if err := s.db.SelectContext(ctx, &rows, q); err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(rows))
	for _, r := range rows {
		e, err := r.entry()
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
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

func (r versionRow) version(withContent bool) (Version, error) {
	v := Version{EntryID: r.EntryID, Version: r.Version, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, Message: r.Message}
	if err := json.Unmarshal([]byte(r.Changed), &v.Changed); err != nil {
		return v, err
	}
	if withContent {
		v.Content = &Content{}
		if err := json.Unmarshal([]byte(r.Content), v.Content); err != nil {
			return v, err
		}
	}
	return v, nil
}

// Versions lists an entry's versions, newest first, without content.
func (s *Store) Versions(ctx context.Context, id string) ([]Version, error) {
	var rows []versionRow
	err := s.db.SelectContext(ctx, &rows,
		"SELECT `entry_id`, `version`, `created_at`, `created_by`, `message`, `changed`, '' AS `content` FROM `entry_versions` WHERE `entry_id` = ? ORDER BY `version` DESC", id)
	if err != nil {
		return nil, err
	}
	versions := make([]Version, 0, len(rows))
	for _, r := range rows {
		v, err := r.version(false)
		if err != nil {
			return nil, err
		}
		versions = append(versions, v)
	}
	return versions, nil
}

// Version returns one version with its content, or nil if absent.
func (s *Store) Version(ctx context.Context, id string, n int) (*Version, error) {
	var row versionRow
	err := s.db.GetContext(ctx, &row,
		"SELECT `entry_id`, `version`, `created_at`, `created_by`, `message`, `changed`, `content` FROM `entry_versions` WHERE `entry_id` = ? AND `version` = ?", id, n)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v, err := row.version(true)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// SetDeleted marks an entry deleted at the given time, or undeletes it when at is nil.
func (s *Store) SetDeleted(ctx context.Context, id string, at *time.Time, by string) error {
	if at == nil {
		by = ""
	}
	_, err := s.db.ExecContext(ctx, "UPDATE `entries` SET `deleted_at` = ?, `deleted_by` = ? WHERE `id` = ?", utcPtr(at), by, id)
	if isUniqueViolation(err) {
		return ErrNameTaken
	}
	return err
}

// Prune deletes old versions and returns how many were removed. A version goes
// when it is not among the newest `keep` (if keep > 0) or is older than
// olderThan (if non-zero). The active version and version 1 are never removed.
func (s *Store) Prune(ctx context.Context, keep int, olderThan time.Time) (int64, error) {
	if keep <= 0 && olderThan.IsZero() {
		return 0, nil
	}
	var conds []string
	var args []interface{}
	if keep > 0 {
		conds = append(conds, "(SELECT COUNT(*) FROM `entry_versions` n WHERE n.`entry_id` = `entry_versions`.`entry_id` AND n.`version` > `entry_versions`.`version`) >= ?")
		args = append(args, keep)
	}
	if !olderThan.IsZero() {
		conds = append(conds, "`created_at` < ?")
		args = append(args, olderThan.UTC())
	}
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM `entry_versions` WHERE `version` > 1"+
			" AND `version` <> (SELECT `active_version` FROM `entries` WHERE `entries`.`id` = `entry_versions`.`entry_id`)"+
			" AND ("+strings.Join(conds, " OR ")+")", args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Meta reads a library_meta value.
func (s *Store) Meta(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.GetContext(ctx, &value, "SELECT `value` FROM `library_meta` WHERE `key` = ?", key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetMeta writes a library_meta value inside the caller's transaction.
func (s *Store) SetMeta(ctx context.Context, tx *sqlx.Tx, key, value string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO `library_meta` (`key`, `value`) VALUES (?, ?) ON CONFLICT(`key`) DO UPDATE SET `value` = excluded.`value`", key, value)
	return err
}
