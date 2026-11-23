package library

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	apierrors "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/share/random"
)

const (
	defaultListLimit = 100
	maxListLimit     = 500
)

// SaveInput is a save request: the new content plus the version it was edited from.
type SaveInput struct {
	Content
	BaseVersion int    `json:"base_version"`
	Message     string `json:"message"`
}

type ListOptions struct {
	Name    string // case-insensitive substring of the name
	Tag     string
	OS      string
	Deleted *bool // true lists only deleted entries; otherwise deleted entries are hidden
	Builtin *bool
	Sort    string // "name" (default) or "-updated_at"
	Offset  int
	Limit   int
}

// Manager implements the library rules on top of the Store.
type Manager struct {
	store *Store
	now   func() time.Time
}

func NewManager(store *Store, now func() time.Time) *Manager {
	return &Manager{store: store, now: now}
}

func apiErr(status int, format string, args ...interface{}) error {
	return apierrors.APIError{Message: fmt.Sprintf(format, args...), HTTPStatus: status}
}

func osKeys(c Content) []string {
	keys := []string{}
	for _, k := range AllOS {
		if _, ok := c.Implementations[k]; ok {
			keys = append(keys, k)
		}
	}
	return keys
}

func contentOf(e Entry) Content {
	return Content{Name: e.Name, Description: e.Description, Tags: e.Tags, Implementations: e.Implementations}
}

func (m *Manager) Create(ctx context.Context, c Content, message, user string) (*Entry, error) {
	Normalize(&c)
	if err := Validate(c); err != nil {
		return nil, err
	}
	id, err := random.UUID4()
	if err != nil {
		return nil, err
	}
	now := m.now().UTC()
	e := Entry{
		ID: id, Name: c.Name, Description: c.Description, Tags: c.Tags, ActiveVersion: 1, OS: osKeys(c),
		CreatedAt: now, CreatedBy: user, UpdatedAt: now, UpdatedBy: user,
	}
	v := Version{EntryID: id, Version: 1, CreatedAt: now, CreatedBy: user, Message: message, Changed: Changed(Content{}, c), Content: &c}
	if err := m.store.Insert(ctx, e, v); err != nil {
		if errors.Is(err, ErrNameTaken) {
			return nil, apiErr(http.StatusConflict, "an entry named %q already exists", c.Name)
		}
		return nil, err
	}
	e.Implementations = c.Implementations
	return &e, nil
}

// live returns an existing, non-deleted entry or a 404.
func (m *Manager) live(ctx context.Context, id string) (*Entry, error) {
	return m.get(ctx, id, false)
}

func (m *Manager) get(ctx context.Context, id string, includeDeleted bool) (*Entry, error) {
	e, err := m.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if e == nil || (e.DeletedAt != nil && !includeDeleted) {
		return nil, apiErr(http.StatusNotFound, "library entry %q not found", id)
	}
	return e, nil
}

// editable is live plus not built-in.
func (m *Manager) editable(ctx context.Context, id string) (*Entry, error) {
	e, err := m.live(ctx, id)
	if err != nil {
		return nil, err
	}
	if e.BuiltinKey != nil {
		return nil, apiErr(http.StatusForbidden, "built-in entry %q can't be changed", e.Name)
	}
	return e, nil
}

func (m *Manager) conflict(ctx context.Context, id string, base int) error {
	e, err := m.live(ctx, id)
	if err != nil {
		return err
	}
	v, err := m.store.Version(ctx, id, e.ActiveVersion)
	if err != nil {
		return err
	}
	by, at := e.UpdatedBy, e.UpdatedAt
	if v != nil {
		by, at = v.CreatedBy, v.CreatedAt
	}
	return apiErr(http.StatusConflict, "entry %q was saved by %s at %s (version %d) after version %d you edited",
		e.Name, by, at.UTC().Format(time.RFC3339), e.ActiveVersion, base)
}

// addVersion stores c as the next version of cur, or returns cur if nothing changed.
func (m *Manager) addVersion(ctx context.Context, cur *Entry, c Content, base int, message, user string) (*Entry, error) {
	changed := Changed(contentOf(*cur), c)
	if len(changed) == 0 {
		return cur, nil
	}
	if base != cur.ActiveVersion {
		return nil, m.conflict(ctx, cur.ID, base)
	}
	now := m.now().UTC()
	next := *cur
	next.Name, next.Description, next.Tags, next.OS = c.Name, c.Description, c.Tags, osKeys(c)
	next.ActiveVersion = base + 1
	next.UpdatedAt, next.UpdatedBy = now, user
	next.Implementations = c.Implementations
	v := Version{EntryID: cur.ID, Version: next.ActiveVersion, CreatedAt: now, CreatedBy: user, Message: message, Changed: changed, Content: &c}
	if err := m.store.AddVersion(ctx, next, v, base); err != nil {
		switch {
		case errors.Is(err, ErrNameTaken):
			return nil, apiErr(http.StatusConflict, "an entry named %q already exists", c.Name)
		case errors.Is(err, ErrConflict):
			return nil, m.conflict(ctx, cur.ID, base)
		}
		return nil, err
	}
	return &next, nil
}

func (m *Manager) Save(ctx context.Context, id string, in SaveInput, user string) (*Entry, error) {
	cur, err := m.editable(ctx, id)
	if err != nil {
		return nil, err
	}
	c := in.Content
	Normalize(&c)
	if err := Validate(c); err != nil {
		return nil, err
	}
	return m.addVersion(ctx, cur, c, in.BaseVersion, in.Message, user)
}

func (m *Manager) Get(ctx context.Context, id string, includeDeleted bool) (*Entry, error) {
	return m.get(ctx, id, includeDeleted)
}

func (m *Manager) List(ctx context.Context, opts ListOptions) ([]Entry, int, error) {
	all, err := m.store.List(ctx, true)
	if err != nil {
		return nil, 0, err
	}
	wantDeleted := opts.Deleted != nil && *opts.Deleted
	name := strings.ToLower(opts.Name)
	var out []Entry
	for _, e := range all {
		if (e.DeletedAt != nil) != wantDeleted {
			continue
		}
		if opts.Builtin != nil && (e.BuiltinKey != nil) != *opts.Builtin {
			continue
		}
		if name != "" && !strings.Contains(strings.ToLower(e.Name), name) {
			continue
		}
		if opts.Tag != "" && !contains(e.Tags, opts.Tag) {
			continue
		}
		if opts.OS != "" && !contains(e.OS, opts.OS) {
			continue
		}
		out = append(out, e)
	}
	if opts.Sort == "-updated_at" {
		sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	}
	total := len(out)
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	start := opts.Offset
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	page := out[start:end]
	if page == nil {
		page = []Entry{}
	}
	return page, total, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (m *Manager) Versions(ctx context.Context, id string) ([]Version, error) {
	if _, err := m.get(ctx, id, true); err != nil {
		return nil, err
	}
	return m.store.Versions(ctx, id)
}

func (m *Manager) Version(ctx context.Context, id string, n int) (*Version, error) {
	if _, err := m.get(ctx, id, true); err != nil {
		return nil, err
	}
	v, err := m.store.Version(ctx, id, n)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, apiErr(http.StatusNotFound, "version %d of library entry %q not found", n, id)
	}
	return v, nil
}

func (m *Manager) Restore(ctx context.Context, id string, n, baseVersion int, user string) (*Entry, error) {
	cur, err := m.editable(ctx, id)
	if err != nil {
		return nil, err
	}
	old, err := m.Version(ctx, id, n)
	if err != nil {
		return nil, err
	}
	c := *old.Content
	Normalize(&c)
	if err := Validate(c); err != nil {
		return nil, err
	}
	return m.addVersion(ctx, cur, c, baseVersion, fmt.Sprintf("Restored version %d", n), user)
}

func (m *Manager) Delete(ctx context.Context, id, user string) error {
	e, err := m.editable(ctx, id)
	if err != nil {
		return err
	}
	at := m.now().UTC()
	return m.store.SetDeleted(ctx, e.ID, &at, user)
}

func (m *Manager) Undelete(ctx context.Context, id, user string) (*Entry, error) {
	e, err := m.get(ctx, id, true)
	if err != nil {
		return nil, err
	}
	if e.DeletedAt == nil {
		return e, nil
	}
	if err := m.store.SetDeleted(ctx, id, nil, user); err != nil {
		if errors.Is(err, ErrNameTaken) {
			return nil, apiErr(http.StatusConflict, "an entry named %q already exists", e.Name)
		}
		return nil, err
	}
	return m.get(ctx, id, false)
}

// Prune removes old versions: all but the newest `keep` (0 = no limit) and any
// older than `days` days (0 = no limit). Returns how many were removed.
func (m *Manager) Prune(ctx context.Context, keep, days int) (int64, error) {
	var olderThan time.Time
	if days > 0 {
		olderThan = m.now().AddDate(0, 0, -days)
	}
	return m.store.Prune(ctx, keep, olderThan)
}
