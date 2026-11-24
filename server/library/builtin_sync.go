package library

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/openrport/openrport/share/random"
)

const builtinAuthor = "procy"

func builtinSuffixed(name string) string { return name + " (built-in)" }

// SyncBuiltins makes sure every shipped built-in has an entry: missing ones are
// created, changed ones get a new version. Deleted built-ins stay deleted and
// user entries are never touched. A user entry using a built-in's name makes
// the built-in "<name> (built-in)". It syncs the tenant carried by ctx.
func SyncBuiltins(ctx context.Context, store Repository, builtins []Builtin, version string, now time.Time) (created, updated int, skipped []string, err error) {
	if _, err := tenantOf(ctx); err != nil {
		return 0, 0, nil, err
	}
	var errs []error
	for _, b := range builtins {
		existing, gerr := store.ByBuiltinKey(ctx, b.Key)
		if gerr != nil {
			errs = append(errs, fmt.Errorf("built-in %s: %w", b.Key, gerr))
			continue
		}
		switch {
		case existing == nil:
			if cerr := createBuiltin(ctx, store, b, version, now); cerr != nil {
				errs = append(errs, fmt.Errorf("built-in %s: %w", b.Key, cerr))
				continue
			}
			created++
		case existing.DeletedAt != nil:
			skipped = append(skipped, b.Key)
		default:
			ok, uerr := updateBuiltin(ctx, store, existing, b, version, now)
			if uerr != nil {
				errs = append(errs, fmt.Errorf("built-in %s: %w", b.Key, uerr))
				continue
			}
			if ok {
				updated++
			}
		}
	}
	return created, updated, skipped, errors.Join(errs...)
}

// freeName picks the first of the candidates no other live entry uses; "" if none.
func freeName(ctx context.Context, store Repository, id string, candidates ...string) (string, error) {
	live, err := store.ListLive(ctx)
	if err != nil {
		return "", err
	}
	for _, c := range candidates {
		taken := false
		for _, o := range live {
			if o.ID != id && o.Name == c {
				taken = true
				break
			}
		}
		if !taken {
			return c, nil
		}
	}
	return "", nil
}

func createBuiltin(ctx context.Context, store Repository, b Builtin, version string, now time.Time) error {
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
func updateBuiltin(ctx context.Context, store Repository, e *Entry, b Builtin, version string, now time.Time) (bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		shipped := b.Content
		if e.Name != shipped.Name && e.Name != builtinSuffixed(shipped.Name) {
			name, err := freeName(ctx, store, e.ID, shipped.Name, builtinSuffixed(shipped.Name))
			if err != nil {
				return false, err
			}
			if name == "" {
				name = e.Name
			}
			shipped.Name = name
		} else {
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
