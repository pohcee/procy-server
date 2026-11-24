package library

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/migration/library"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/share/tenant"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	db, err := sqlite.New(filepath.Join(t.TempDir(), "library.db"), library.FS, sqlite.DataSourceOptions{WALEnabled: false})
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewSQLiteStore(db)
}

// testCtx carries the default tenant, as the server context does on a standalone server.
var testCtx = tenant.WithID(context.Background(), tenant.Default)

var t0 = time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

func testContent(name string) *Content {
	return &Content{
		Name:        name,
		Description: "desc",
		Tags:        []string{"a", "b"},
		Implementations: map[string]Implementation{
			OSLinux:   {Kind: KindScript, Code: "echo hi", Interpreter: "bash", IsSudo: true, Cwd: "/tmp", TimeoutSec: 30},
			OSWindows: {Kind: KindCommand, Code: "dir", TimeoutSec: 60},
		},
	}
}

func testEntry(id, name string) (Entry, Version) {
	c := testContent(name)
	e := Entry{
		ID: id, Name: name, Description: c.Description, Tags: c.Tags, ActiveVersion: 1,
		OS:        []string{OSLinux, OSWindows},
		CreatedAt: t0, CreatedBy: "u1", UpdatedAt: t0, UpdatedBy: "u1",
	}
	v := Version{EntryID: id, Version: 1, CreatedAt: t0, CreatedBy: "u1", Message: "first", Changed: []string{"name"}, Content: c}
	return e, v
}

func addVersion(t *testing.T, s *SQLiteStore, id string, n int, at time.Time) {
	t.Helper()
	addVersionIn(t, s, testCtx, id, n, at)
}

func addVersionIn(t *testing.T, s *SQLiteStore, ctx context.Context, id string, n int, at time.Time) {
	t.Helper()
	c := testContent("A")
	c.Description = fmt.Sprintf("d%d", n)
	e := Entry{ID: id, Name: "A", Description: c.Description, Tags: c.Tags, ActiveVersion: n, OS: []string{OSLinux, OSWindows}, UpdatedAt: at, UpdatedBy: "u2"}
	v := Version{EntryID: id, Version: n, CreatedAt: at, CreatedBy: "u2", Message: "m", Changed: []string{"description"}, Content: c}
	require.NoError(t, s.AddVersion(ctx, e, v, n-1))
}

func versionNumbers(t *testing.T, s *SQLiteStore, id string) []int {
	t.Helper()
	return versionNumbersIn(t, s, testCtx, id)
}

func versionNumbersIn(t *testing.T, s *SQLiteStore, ctx context.Context, id string) []int {
	t.Helper()
	vs, err := s.Versions(ctx, id)
	require.NoError(t, err)
	nums := []int{}
	for _, v := range vs {
		nums = append(nums, v.Version)
	}
	return nums
}

func TestStoreInsertAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx
	e, v := testEntry("id1", "A")
	require.NoError(t, s.Insert(ctx, e, v))

	got, err := s.Get(ctx, "id1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "A", got.Name)
	assert.Equal(t, []string{"a", "b"}, got.Tags)
	assert.Equal(t, []string{OSLinux, OSWindows}, got.OS)
	assert.Equal(t, 1, got.ActiveVersion)
	assert.True(t, t0.Equal(got.CreatedAt))
	assert.Nil(t, got.DeletedAt)
	assert.Equal(t, testContent("A").Implementations, got.Implementations)

	missing, err := s.Get(ctx, "nope")
	require.NoError(t, err)
	assert.Nil(t, missing)

	list, err := s.List(ctx, false)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Empty(t, list[0].Implementations)

	gv, err := s.Version(ctx, "id1", 1)
	require.NoError(t, err)
	require.NotNil(t, gv)
	assert.Equal(t, testContent("A"), gv.Content)
	assert.Equal(t, []string{"name"}, gv.Changed)

	vs, err := s.Versions(ctx, "id1")
	require.NoError(t, err)
	require.Len(t, vs, 1)
	assert.Nil(t, vs[0].Content)
}

func TestStoreAddVersionConflict(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx
	e, v := testEntry("id1", "A")
	require.NoError(t, s.Insert(ctx, e, v))

	e2 := e
	e2.ActiveVersion = 2
	e2.Description = "changed"
	v2 := v
	v2.Version = 2
	require.NoError(t, s.AddVersion(ctx, e2, v2, 1))
	assert.ErrorIs(t, s.AddVersion(ctx, e2, v2, 1), ErrConflict)

	got, err := s.Get(ctx, "id1")
	require.NoError(t, err)
	assert.Equal(t, 2, got.ActiveVersion)
	assert.Equal(t, "changed", got.Description)
	assert.Equal(t, []int{2, 1}, versionNumbers(t, s, "id1"))
}

func TestStoreUniqueLiveName(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx
	e1, v1 := testEntry("id1", "A")
	require.NoError(t, s.Insert(ctx, e1, v1))
	e2, v2 := testEntry("id2", "A")
	assert.ErrorIs(t, s.Insert(ctx, e2, v2), ErrNameTaken)

	// failed insert left nothing behind
	got, err := s.Get(ctx, "id2")
	require.NoError(t, err)
	assert.Nil(t, got)

	require.NoError(t, s.SetDeleted(ctx, "id1", &t0, "u1"))
	require.NoError(t, s.Insert(ctx, e2, v2))

	live, err := s.List(ctx, false)
	require.NoError(t, err)
	require.Len(t, live, 1)
	assert.Equal(t, "id2", live[0].ID)
	all, err := s.List(ctx, true)
	require.NoError(t, err)
	assert.Len(t, all, 2)

	deleted, err := s.Get(ctx, "id1")
	require.NoError(t, err)
	require.NotNil(t, deleted.DeletedAt)
	assert.Equal(t, "u1", deleted.DeletedBy)

	assert.ErrorIs(t, s.SetDeleted(ctx, "id1", nil, ""), ErrNameTaken)
	require.NoError(t, s.SetDeleted(ctx, "id2", &t0, "u1"))
	require.NoError(t, s.SetDeleted(ctx, "id1", nil, ""))
	back, err := s.Get(ctx, "id1")
	require.NoError(t, err)
	assert.Nil(t, back.DeletedAt)
	assert.Equal(t, "", back.DeletedBy)
}

func TestStorePruneKeepsActiveAndFirst(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx
	e, v := testEntry("id1", "A")
	require.NoError(t, s.Insert(ctx, e, v))
	for n := 2; n <= 6; n++ {
		addVersion(t, s, "id1", n, t0.Add(time.Duration(n)*time.Hour))
	}

	n, err := s.Prune(ctx, 0, time.Time{})
	require.NoError(t, err)
	assert.EqualValues(t, 0, n)
	assert.Len(t, versionNumbers(t, s, "id1"), 6)

	n, err = s.Prune(ctx, 2, time.Time{})
	require.NoError(t, err)
	assert.EqualValues(t, 3, n)
	assert.Equal(t, []int{6, 5, 1}, versionNumbers(t, s, "id1"))
}

func TestStorePruneByAge(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx
	e, v := testEntry("id1", "A")
	require.NoError(t, s.Insert(ctx, e, v))
	for n := 2; n <= 5; n++ {
		addVersion(t, s, "id1", n, t0.AddDate(0, 0, n))
	}

	// versions 2 and 3 are older than the cutoff; 4 is not; 5 is active
	n, err := s.Prune(ctx, 0, t0.AddDate(0, 0, 4))
	require.NoError(t, err)
	assert.EqualValues(t, 2, n)
	assert.Equal(t, []int{5, 4, 1}, versionNumbers(t, s, "id1"))

	// everything old: only the active version and version 1 remain
	n, err = s.Prune(ctx, 0, t0.AddDate(1, 0, 0))
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	assert.Equal(t, []int{5, 1}, versionNumbers(t, s, "id1"))
}

func TestStoreMeta(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx
	_, ok, err := s.Meta(ctx, "k")
	require.NoError(t, err)
	assert.False(t, ok)

	tx, err := s.db.BeginTxx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, s.setMeta(ctx, tx, "k", "v"))
	require.NoError(t, s.setMeta(ctx, tx, "k", "v2"))
	require.NoError(t, tx.Commit())

	val, ok, err := s.Meta(ctx, "k")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "v2", val)
}
