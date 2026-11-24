package library

import (
	"context"
	"errors"
	"time"
)

var (
	ErrConflict  = errors.New("library entry was changed by someone else")
	ErrNameTaken = errors.New("a library entry with this name already exists")
)

// Repository persists entries and their versions. Every method is scoped to the tenant carried by its context and
// fails with tenant.ErrMissing when there is none.
type Repository interface {
	// Insert stores a new entry together with its first version. ErrNameTaken when a live entry has the name.
	Insert(ctx context.Context, e Entry, v Version) error
	// AddVersion makes v the active version of e, provided the entry is live and its active version is still
	// baseVersion (ErrConflict otherwise).
	AddVersion(ctx context.Context, e Entry, v Version, baseVersion int) error
	// Get returns the entry with its active version's implementations, or nil if absent.
	Get(ctx context.Context, id string) (*Entry, error)
	// List returns entries without implementations, ordered by name.
	List(ctx context.Context, includeDeleted bool) ([]Entry, error)
	// ListLive returns the live entries with their active version's implementations, ordered by name.
	ListLive(ctx context.Context) ([]Entry, error)
	// Versions lists an entry's versions, newest first, without content.
	Versions(ctx context.Context, id string) ([]Version, error)
	// Version returns one version with its content, or nil if absent.
	Version(ctx context.Context, id string, n int) (*Version, error)
	// SetDeleted marks an entry deleted at the given time, or undeletes it when at is nil.
	SetDeleted(ctx context.Context, id string, at *time.Time, by string) error
	// Prune deletes old versions and returns how many were removed. A version goes when it is not among the newest
	// `keep` (if keep > 0) or is older than olderThan (if non-zero). The active version and version 1 stay.
	Prune(ctx context.Context, keep int, olderThan time.Time) (int64, error)
	// Meta reads a library metadata value.
	Meta(ctx context.Context, key string) (string, bool, error)
	// ByBuiltinKey returns the entry (deleted or not) created for a built-in key, or nil.
	ByBuiltinKey(ctx context.Context, key string) (*Entry, error)
	Close() error
}

// LegacyImporter is implemented by repositories that can read the library tables of servers from before the
// unified library. Others have nothing to import.
type LegacyImporter interface {
	// ImportLegacy copies the old scripts and commands once into entries and returns how many it created.
	ImportLegacy(ctx context.Context) (int, error)
}
