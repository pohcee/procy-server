package library

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testBuiltin(key, name, code string) Builtin {
	return Builtin{Key: key, Content: Content{
		Name: name, Description: "d", Tags: []string{"procy", key},
		Implementations: map[string]Implementation{
			OSLinux: {Kind: KindScript, Code: code, Interpreter: "sh", IsSudo: true, TimeoutSec: 300},
		},
	}}
}

func entryByKey(t *testing.T, s *Store, key string) *Entry {
	t.Helper()
	e, err := s.ByBuiltinKey(testCtx, key)
	require.NoError(t, err)
	return e
}

func TestSyncCreatesBuiltins(t *testing.T) {
	s := newTestStore(t)
	bs, err := Builtins()
	require.NoError(t, err)

	created, updated, _, err := SyncBuiltins(testCtx, s, bs, "1.2.3", t0)
	require.NoError(t, err)
	assert.Equal(t, len(bs), created)
	assert.Equal(t, 0, updated)

	e := entryByKey(t, s, "os-updates")
	require.NotNil(t, e)
	assert.Equal(t, "OS updates", e.Name)
	assert.Equal(t, 1, e.ActiveVersion)
	assert.Equal(t, []string{OSLinux, OSWindows}, e.OS)
	vs, err := s.Versions(testCtx, e.ID)
	require.NoError(t, err)
	require.Len(t, vs, 1)
	assert.Equal(t, "procy", vs[0].CreatedBy)
	assert.Equal(t, "Added with Procy 1.2.3", vs[0].Message)
	assert.Equal(t, "procy", e.CreatedBy)
}

func TestSyncIdempotent(t *testing.T) {
	s := newTestStore(t)
	bs := []Builtin{testBuiltin("a", "A", "echo 1")}
	_, _, _, err := SyncBuiltins(testCtx, s, bs, "1", t0)
	require.NoError(t, err)
	created, updated, _, err := SyncBuiltins(testCtx, s, bs, "1", t0)
	require.NoError(t, err)
	assert.Equal(t, 0, created)
	assert.Equal(t, 0, updated)
	assert.Equal(t, 1, entryByKey(t, s, "a").ActiveVersion)
}

func TestSyncUpdatesChangedBuiltin(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx
	_, _, _, err := SyncBuiltins(ctx, s, []Builtin{testBuiltin("a", "A", "echo 1")}, "1", t0)
	require.NoError(t, err)

	created, updated, _, err := SyncBuiltins(ctx, s, []Builtin{testBuiltin("a", "A", "echo 2")}, "2", t0.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 0, created)
	assert.Equal(t, 1, updated)

	e := entryByKey(t, s, "a")
	assert.Equal(t, 2, e.ActiveVersion)
	assert.Equal(t, "echo 2", e.Implementations[OSLinux].Code)
	vs, err := s.Versions(ctx, e.ID)
	require.NoError(t, err)
	require.Len(t, vs, 2)
	var v2 Version
	for _, v := range vs {
		if v.Version == 2 {
			v2 = v
		}
	}
	assert.Equal(t, "procy", v2.CreatedBy)
	assert.Equal(t, "Updated with Procy 2", v2.Message)
	assert.Equal(t, []string{"linux"}, v2.Changed)
}

func TestSyncNameClash(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx
	e, v := testEntry("user1", "A")
	require.NoError(t, s.Insert(ctx, e, v))

	bs := []Builtin{testBuiltin("a", "A", "echo 1")}
	created, _, _, err := SyncBuiltins(ctx, s, bs, "1", t0)
	require.NoError(t, err)
	assert.Equal(t, 1, created)
	b := entryByKey(t, s, "a")
	assert.Equal(t, "A (built-in)", b.Name)

	// the user's entry is untouched, and later syncs keep the suffixed name stable
	u, err := s.Get(ctx, "user1")
	require.NoError(t, err)
	assert.Equal(t, "A", u.Name)
	created, updated, _, err := SyncBuiltins(ctx, s, bs, "1", t0)
	require.NoError(t, err)
	assert.Equal(t, 0, created+updated)

	// an update keeps the suffixed name too
	_, updated, _, err = SyncBuiltins(ctx, s, []Builtin{testBuiltin("a", "A", "echo 2")}, "2", t0)
	require.NoError(t, err)
	assert.Equal(t, 1, updated)
	assert.Equal(t, "A (built-in)", entryByKey(t, s, "a").Name)
}

func TestSyncKeepsDeletedBuiltinDeleted(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx
	_, _, _, err := SyncBuiltins(ctx, s, []Builtin{testBuiltin("a", "A", "echo 1")}, "1", t0)
	require.NoError(t, err)
	e := entryByKey(t, s, "a")
	at := t0.Add(time.Hour)
	require.NoError(t, s.SetDeleted(ctx, e.ID, &at, "admin"))

	created, updated, skipped, err := SyncBuiltins(ctx, s, []Builtin{testBuiltin("a", "A", "echo 2")}, "2", t0)
	require.NoError(t, err)
	assert.Equal(t, 0, created+updated)
	assert.Equal(t, []string{"a"}, skipped)
	e = entryByKey(t, s, "a")
	require.NotNil(t, e.DeletedAt)
	assert.Equal(t, 1, e.ActiveVersion)
	live, err := s.ListLive(ctx)
	require.NoError(t, err)
	assert.Empty(t, live)
}

func TestSyncOneFailureDoesNotStopTheRest(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx
	for i, n := range []string{"A", "A (built-in)"} {
		e, v := testEntry("u"+string(rune('1'+i)), n)
		require.NoError(t, s.Insert(ctx, e, v))
	}
	bs := []Builtin{testBuiltin("a", "A", "x"), testBuiltin("b", "B", "y")}
	created, _, _, err := SyncBuiltins(ctx, s, bs, "1", t0)
	require.Error(t, err)
	assert.Equal(t, 1, created)
	assert.Nil(t, entryByKey(t, s, "a"))
	assert.NotNil(t, entryByKey(t, s, "b"))
}

func TestSyncUpdateNeverRenamesIntoClash(t *testing.T) {
	s := newTestStore(t)
	ctx := testCtx
	_, _, _, err := SyncBuiltins(ctx, s, []Builtin{testBuiltin("a", "Old", "x")}, "1", t0)
	require.NoError(t, err)
	e, v := testEntry("u1", "New")
	require.NoError(t, s.Insert(ctx, e, v))

	_, updated, _, err := SyncBuiltins(ctx, s, []Builtin{testBuiltin("a", "New", "x")}, "2", t0)
	require.NoError(t, err)
	assert.Equal(t, 1, updated)
	assert.Equal(t, "New (built-in)", entryByKey(t, s, "a").Name)
}

func TestSyncIdempotentRealBuiltins(t *testing.T) {
	s := newTestStore(t)
	bs, err := Builtins()
	require.NoError(t, err)
	_, _, _, err = SyncBuiltins(testCtx, s, bs, "1", t0)
	require.NoError(t, err)
	created, updated, _, err := SyncBuiltins(testCtx, s, bs, "1", t0)
	require.NoError(t, err)
	assert.Equal(t, 0, created+updated)
}
