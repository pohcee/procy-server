package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/openrport/openrport/share/random"
)

const builtinAuthor = "procy"

// ByBuiltinKey returns the entry (deleted or not) created for a built-in key, or nil.
func (s *Store) ByBuiltinKey(ctx context.Context, key string) (*Entry, error) {
	var id string
	err := s.db.GetContext(ctx, &id, "SELECT `id` FROM `entries` WHERE `builtin_key` = ?", key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

func builtinSuffixed(name string) string { return name + " (built-in)" }

// SyncBuiltins makes sure every shipped built-in has an entry: missing ones are
// created, changed ones get a new version. Deleted built-ins stay deleted and
// user entries are never touched. A user entry using a built-in's name makes
// the built-in "<name> (built-in)".
func SyncBuiltins(ctx context.Context, store *Store, builtins []Builtin, version string, now time.Time) (created, updated int, err error) {
	for _, b := range builtins {
		existing, err := store.ByBuiltinKey(ctx, b.Key)
		if err != nil {
			return created, updated, fmt.Errorf("built-in %s: %w", b.Key, err)
		}
		switch {
		case existing == nil:
			if err := createBuiltin(ctx, store, b, version, now); err != nil {
				return created, updated, fmt.Errorf("built-in %s: %w", b.Key, err)
			}
			created++
		case existing.DeletedAt != nil:
			continue
		default:
			ok, err := updateBuiltin(ctx, store, existing, b, version, now)
			if err != nil {
				return created, updated, fmt.Errorf("built-in %s: %w", b.Key, err)
			}
			if ok {
				updated++
			}
		}
	}
	return created, updated, nil
}

func createBuiltin(ctx context.Context, store *Store, b Builtin, version string, now time.Time) error {
	id, err := random.UUID4()
	if err != nil {
		return err
	}
	c := b.Content
	for attempt := 0; ; attempt++ {
		key := b.Key
		e := Entry{
			ID: id, Name: c.Name, Description: c.Description, Tags: c.Tags, ActiveVersion: 1, BuiltinKey: &key,
			OS: osKeys(c), CreatedAt: now, CreatedBy: builtinAuthor, UpdatedAt: now, UpdatedBy: builtinAuthor,
		}
		v := Version{
			EntryID: id, Version: 1, CreatedAt: now, CreatedBy: builtinAuthor,
			Message: "Added with Procy " + version, Changed: Changed(Content{}, c), Content: &c,
		}
		err := store.Insert(ctx, e, v)
		if errors.Is(err, ErrNameTaken) && attempt == 0 {
			c.Name = builtinSuffixed(b.Content.Name)
			continue
		}
		return err
	}
}

// updateBuiltin adds a version when the shipped content differs from the active
// one. It reports whether a version was added.
func updateBuiltin(ctx context.Context, store *Store, e *Entry, b Builtin, version string, now time.Time) (bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		shipped := b.Content
		if e.Name == builtinSuffixed(shipped.Name) {
			shipped.Name = e.Name
		}
		changed := Changed(contentOf(*e), shipped)
		if len(changed) == 0 {
			return false, nil
		}
		next := *e
		next.Name, next.Description, next.Tags, next.OS = shipped.Name, shipped.Description, shipped.Tags, osKeys(shipped)
		next.ActiveVersion = e.ActiveVersion + 1
		next.UpdatedAt, next.UpdatedBy = now, builtinAuthor
		v := Version{
			EntryID: e.ID, Version: next.ActiveVersion, CreatedAt: now, CreatedBy: builtinAuthor,
			Message: "Updated with Procy " + version, Changed: changed, Content: &shipped,
		}
		err := store.AddVersion(ctx, next, v, e.ActiveVersion)
		if errors.Is(err, ErrConflict) {
			fresh, gerr := store.Get(ctx, e.ID)
			if gerr != nil {
				return false, gerr
			}
			if fresh == nil || fresh.DeletedAt != nil {
				return false, nil
			}
			e = fresh
			continue
		}
		return err == nil, err
	}
	return false, ErrConflict
}
