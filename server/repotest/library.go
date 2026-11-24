package repotest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/share/tenant"
)

var libT0 = time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

func libContent(name string) *library.Content {
	return &library.Content{
		Name:        name,
		Description: "desc",
		Tags:        []string{"a", "b"},
		Implementations: map[string]library.Implementation{
			library.OSLinux:   {Kind: library.KindScript, Code: "echo hi", Interpreter: "bash", IsSudo: true, Cwd: "/tmp", TimeoutSec: 30},
			library.OSWindows: {Kind: library.KindCommand, Code: "dir", TimeoutSec: 60},
		},
	}
}

func libEntry(id, name string) (library.Entry, library.Version) {
	c := libContent(name)
	e := library.Entry{
		ID: id, Name: name, Description: c.Description, Tags: c.Tags, ActiveVersion: 1,
		OS:        []string{library.OSLinux, library.OSWindows},
		CreatedAt: libT0, CreatedBy: "u1", UpdatedAt: libT0, UpdatedBy: "u1",
	}
	v := library.Version{EntryID: id, Version: 1, CreatedAt: libT0, CreatedBy: "u1", Message: "first", Changed: []string{"name"}, Content: c}
	return e, v
}

// libAddVersion makes version n (based on n-1) the active version of entry id, named "A".
func libAddVersion(ctx context.Context, t *testing.T, r library.Repository, id string, n int, at time.Time) {
	t.Helper()
	c := libContent("A")
	c.Description = fmt.Sprintf("d%d", n)
	e := library.Entry{ID: id, Name: "A", Description: c.Description, Tags: c.Tags, ActiveVersion: n, OS: []string{library.OSLinux, library.OSWindows}, UpdatedAt: at, UpdatedBy: "u2"}
	v := library.Version{EntryID: id, Version: n, CreatedAt: at, CreatedBy: "u2", Message: "m", Changed: []string{"description"}, Content: c}
	require.NoError(t, r.AddVersion(ctx, e, v, n-1))
}

func libVersionNumbers(ctx context.Context, t *testing.T, r library.Repository, id string) []int {
	t.Helper()
	vs, err := r.Versions(ctx, id)
	require.NoError(t, err)
	nums := []int{}
	for _, v := range vs {
		nums = append(nums, v.Version)
	}
	return nums
}

func libNames(entries []library.Entry) []string {
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names
}

// Library runs the contract tests of library.Repository.
func Library(t *testing.T, newRepo Factory[library.Repository]) {
	a, b := Ctx(TenantA), Ctx(TenantB)

	t.Run("RequiresTenant", func(t *testing.T) {
		r := newRepo(t)
		ctx := context.Background()
		e, v := libEntry("id1", "A")

		assert.ErrorIs(t, r.Insert(ctx, e, v), tenant.ErrMissing)
		assert.ErrorIs(t, r.AddVersion(ctx, e, v, 1), tenant.ErrMissing)
		_, err := r.Get(ctx, "id1")
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = r.List(ctx, true)
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = r.ListLive(ctx)
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = r.Versions(ctx, "id1")
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = r.Version(ctx, "id1", 1)
		assert.ErrorIs(t, err, tenant.ErrMissing)
		at := libT0
		assert.ErrorIs(t, r.SetDeleted(ctx, "id1", &at, "u"), tenant.ErrMissing)
		_, err = r.Prune(ctx, 1, time.Time{})
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, _, err = r.Meta(ctx, "k")
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = r.ByBuiltinKey(ctx, "k")
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, _, _, err = library.SyncBuiltins(ctx, r, []library.Builtin{libBuiltin("k1", "B", "echo")}, "1", libT0)
		assert.ErrorIs(t, err, tenant.ErrMissing)
	})

	t.Run("InsertAndGet", func(t *testing.T) {
		r := newRepo(t)
		e, v := libEntry("id1", "A")
		require.NoError(t, r.Insert(a, e, v))

		got, err := r.Get(a, "id1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "id1", got.ID)
		assert.Equal(t, "A", got.Name)
		assert.Equal(t, "desc", got.Description)
		assert.Equal(t, []string{"a", "b"}, got.Tags)
		assert.Equal(t, []string{library.OSLinux, library.OSWindows}, got.OS)
		assert.Equal(t, 1, got.ActiveVersion)
		assert.Nil(t, got.BuiltinKey)
		assert.True(t, libT0.Equal(got.CreatedAt))
		assert.Equal(t, "u1", got.CreatedBy)
		assert.True(t, libT0.Equal(got.UpdatedAt))
		assert.Equal(t, "u1", got.UpdatedBy)
		assert.Nil(t, got.DeletedAt)
		assert.Equal(t, "", got.DeletedBy)
		assert.Equal(t, libContent("A").Implementations, got.Implementations)

		missing, err := r.Get(a, "nope")
		require.NoError(t, err)
		assert.Nil(t, missing)

		list, err := r.List(a, false)
		require.NoError(t, err)
		require.Len(t, list, 1)
		assert.Empty(t, list[0].Implementations)
		assert.Equal(t, []string{"a", "b"}, list[0].Tags)

		gv, err := r.Version(a, "id1", 1)
		require.NoError(t, err)
		require.NotNil(t, gv)
		assert.Equal(t, "id1", gv.EntryID)
		assert.Equal(t, 1, gv.Version)
		assert.True(t, libT0.Equal(gv.CreatedAt))
		assert.Equal(t, "u1", gv.CreatedBy)
		assert.Equal(t, "first", gv.Message)
		assert.Equal(t, libContent("A"), gv.Content)
		assert.Equal(t, []string{"name"}, gv.Changed)

		noVersion, err := r.Version(a, "id1", 2)
		require.NoError(t, err)
		assert.Nil(t, noVersion)

		vs, err := r.Versions(a, "id1")
		require.NoError(t, err)
		require.Len(t, vs, 1)
		assert.Nil(t, vs[0].Content)
		assert.Equal(t, "first", vs[0].Message)
		assert.Equal(t, []string{"name"}, vs[0].Changed)

		none, err := r.Versions(a, "nope")
		require.NoError(t, err)
		assert.Empty(t, none)
	})

	t.Run("EmptyTagsAndChanged", func(t *testing.T) {
		r := newRepo(t)
		e, v := libEntry("id1", "A")
		e.Tags, e.OS, v.Changed = nil, nil, nil
		require.NoError(t, r.Insert(a, e, v))

		got, err := r.Get(a, "id1")
		require.NoError(t, err)
		assert.Empty(t, got.Tags)
		assert.Empty(t, got.OS)
		gv, err := r.Version(a, "id1", 1)
		require.NoError(t, err)
		assert.NotNil(t, gv.Changed)
		assert.Empty(t, gv.Changed)
	})

	t.Run("BuiltinKey", func(t *testing.T) {
		r := newRepo(t)
		e, v := libEntry("id1", "A")
		key := "k1"
		e.BuiltinKey = &key
		require.NoError(t, r.Insert(a, e, v))

		got, err := r.ByBuiltinKey(a, "k1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "id1", got.ID)
		require.NotNil(t, got.BuiltinKey)
		assert.Equal(t, "k1", *got.BuiltinKey)
		assert.Equal(t, libContent("A").Implementations, got.Implementations)

		missing, err := r.ByBuiltinKey(a, "k2")
		require.NoError(t, err)
		assert.Nil(t, missing)

		// deleted entries are still found
		at := libT0
		require.NoError(t, r.SetDeleted(a, "id1", &at, "u1"))
		deleted, err := r.ByBuiltinKey(a, "k1")
		require.NoError(t, err)
		require.NotNil(t, deleted)
		assert.NotNil(t, deleted.DeletedAt)
	})

	t.Run("AddVersionConflict", func(t *testing.T) {
		r := newRepo(t)
		e, v := libEntry("id1", "A")
		require.NoError(t, r.Insert(a, e, v))

		e2 := e
		e2.ActiveVersion = 2
		e2.Description = "changed"
		e2.Tags = []string{"c"}
		e2.OS = []string{library.OSLinux}
		e2.UpdatedAt = libT0.Add(time.Hour)
		e2.UpdatedBy = "u2"
		v2 := v
		v2.Version = 2
		v2.Content = libContent("A")
		v2.Content.Description = "changed"
		require.NoError(t, r.AddVersion(a, e2, v2, 1))
		assert.ErrorIs(t, r.AddVersion(a, e2, v2, 1), library.ErrConflict)

		got, err := r.Get(a, "id1")
		require.NoError(t, err)
		assert.Equal(t, 2, got.ActiveVersion)
		assert.Equal(t, "changed", got.Description)
		assert.Equal(t, []string{"c"}, got.Tags)
		assert.Equal(t, []string{library.OSLinux}, got.OS)
		assert.True(t, libT0.Add(time.Hour).Equal(got.UpdatedAt))
		assert.Equal(t, "u2", got.UpdatedBy)
		assert.Equal(t, "u1", got.CreatedBy, "creation is kept")
		assert.True(t, libT0.Equal(got.CreatedAt))
		assert.Equal(t, []int{2, 1}, libVersionNumbers(a, t, r, "id1"))

		// a missing entry is a conflict too
		e3, v3 := libEntry("nope", "N")
		assert.ErrorIs(t, r.AddVersion(a, e3, v3, 0), library.ErrConflict)
		assert.Empty(t, libVersionNumbers(a, t, r, "nope"))

		// as is a deleted one, and nothing is stored
		at := libT0
		require.NoError(t, r.SetDeleted(a, "id1", &at, "u1"))
		e2.ActiveVersion = 3
		v2.Version = 3
		assert.ErrorIs(t, r.AddVersion(a, e2, v2, 2), library.ErrConflict)
		assert.Equal(t, []int{2, 1}, libVersionNumbers(a, t, r, "id1"))
	})

	t.Run("AddVersionNameTaken", func(t *testing.T) {
		r := newRepo(t)
		e1, v1 := libEntry("id1", "A")
		require.NoError(t, r.Insert(a, e1, v1))
		e2, v2 := libEntry("id2", "B")
		require.NoError(t, r.Insert(a, e2, v2))

		e2.Name = "A"
		e2.ActiveVersion = 2
		v2.Version = 2
		assert.ErrorIs(t, r.AddVersion(a, e2, v2, 1), library.ErrNameTaken)
		got, err := r.Get(a, "id2")
		require.NoError(t, err)
		assert.Equal(t, "B", got.Name)
		assert.Equal(t, 1, got.ActiveVersion)
		assert.Equal(t, []int{1}, libVersionNumbers(a, t, r, "id2"))
	})

	t.Run("UniqueLiveName", func(t *testing.T) {
		r := newRepo(t)
		e1, v1 := libEntry("id1", "A")
		require.NoError(t, r.Insert(a, e1, v1))
		e2, v2 := libEntry("id2", "A")
		assert.ErrorIs(t, r.Insert(a, e2, v2), library.ErrNameTaken)

		// failed insert left nothing behind
		got, err := r.Get(a, "id2")
		require.NoError(t, err)
		assert.Nil(t, got)
		assert.Empty(t, libVersionNumbers(a, t, r, "id2"))

		at := libT0
		require.NoError(t, r.SetDeleted(a, "id1", &at, "u1"))
		require.NoError(t, r.Insert(a, e2, v2))

		live, err := r.List(a, false)
		require.NoError(t, err)
		require.Len(t, live, 1)
		assert.Equal(t, "id2", live[0].ID)
		all, err := r.List(a, true)
		require.NoError(t, err)
		assert.Len(t, all, 2)

		deleted, err := r.Get(a, "id1")
		require.NoError(t, err)
		require.NotNil(t, deleted.DeletedAt)
		assert.True(t, libT0.Equal(*deleted.DeletedAt))
		assert.Equal(t, "u1", deleted.DeletedBy)

		assert.ErrorIs(t, r.SetDeleted(a, "id1", nil, ""), library.ErrNameTaken)
		require.NoError(t, r.SetDeleted(a, "id2", &at, "u1"))
		// undeleting clears who deleted it, whatever is passed
		require.NoError(t, r.SetDeleted(a, "id1", nil, "ignored"))
		back, err := r.Get(a, "id1")
		require.NoError(t, err)
		assert.Nil(t, back.DeletedAt)
		assert.Equal(t, "", back.DeletedBy)

		// setting a missing entry deleted is not an error
		require.NoError(t, r.SetDeleted(a, "nope", &at, "u1"))
	})

	t.Run("ListOrder", func(t *testing.T) {
		r := newRepo(t)
		for _, x := range []struct{ id, name string }{{"id3", "c"}, {"id1", "B"}, {"id2", "a"}} {
			e, v := libEntry(x.id, x.name)
			require.NoError(t, r.Insert(a, e, v))
		}
		at := libT0
		require.NoError(t, r.SetDeleted(a, "id1", &at, "u1"))
		// a live entry sharing the deleted one's name
		e, v := libEntry("id0", "B")
		require.NoError(t, r.Insert(a, e, v))

		all, err := r.List(a, true)
		require.NoError(t, err)
		assert.Equal(t, []string{"a", "B", "B", "c"}, libNames(all))
		assert.Equal(t, "id0", all[1].ID, "same name: ordered by id")
		assert.Equal(t, "id1", all[2].ID)

		live, err := r.List(a, false)
		require.NoError(t, err)
		assert.Equal(t, []string{"a", "B", "c"}, libNames(live))
		assert.Equal(t, "id0", live[1].ID)

		withImpl, err := r.ListLive(a)
		require.NoError(t, err)
		assert.Equal(t, []string{"a", "B", "c"}, libNames(withImpl))
		for _, e := range withImpl {
			assert.Equal(t, libContent(e.Name).Implementations, e.Implementations)
			assert.Nil(t, e.DeletedAt)
		}

		empty, err := r.List(b, true)
		require.NoError(t, err)
		assert.Empty(t, empty)
		emptyLive, err := r.ListLive(b)
		require.NoError(t, err)
		assert.Empty(t, emptyLive)
	})

	t.Run("ListLiveUsesActiveVersion", func(t *testing.T) {
		r := newRepo(t)
		e, v := libEntry("id1", "A")
		require.NoError(t, r.Insert(a, e, v))
		e2 := e
		e2.ActiveVersion = 2
		v2 := v
		v2.Version = 2
		v2.Content = libContent("A")
		v2.Content.Implementations = map[string]library.Implementation{library.OSDarwin: {Kind: library.KindCommand, Code: "ls", TimeoutSec: 5}}
		require.NoError(t, r.AddVersion(a, e2, v2, 1))

		live, err := r.ListLive(a)
		require.NoError(t, err)
		require.Len(t, live, 1)
		assert.Equal(t, v2.Content.Implementations, live[0].Implementations)
		got, err := r.Get(a, "id1")
		require.NoError(t, err)
		assert.Equal(t, v2.Content.Implementations, got.Implementations)
	})

	t.Run("PruneKeepsActiveAndFirst", func(t *testing.T) {
		r := newRepo(t)
		e, v := libEntry("id1", "A")
		require.NoError(t, r.Insert(a, e, v))
		for n := 2; n <= 6; n++ {
			libAddVersion(a, t, r, "id1", n, libT0.Add(time.Duration(n)*time.Hour))
		}

		n, err := r.Prune(a, 0, time.Time{})
		require.NoError(t, err)
		assert.EqualValues(t, 0, n)
		assert.Len(t, libVersionNumbers(a, t, r, "id1"), 6)

		n, err = r.Prune(a, 2, time.Time{})
		require.NoError(t, err)
		assert.EqualValues(t, 3, n)
		assert.Equal(t, []int{6, 5, 1}, libVersionNumbers(a, t, r, "id1"))
	})

	t.Run("PruneByAge", func(t *testing.T) {
		r := newRepo(t)
		e, v := libEntry("id1", "A")
		require.NoError(t, r.Insert(a, e, v))
		for n := 2; n <= 5; n++ {
			libAddVersion(a, t, r, "id1", n, libT0.AddDate(0, 0, n))
		}

		// versions 2 and 3 are older than the cutoff; 4 is not; 5 is active
		n, err := r.Prune(a, 0, libT0.AddDate(0, 0, 4))
		require.NoError(t, err)
		assert.EqualValues(t, 2, n)
		assert.Equal(t, []int{5, 4, 1}, libVersionNumbers(a, t, r, "id1"))

		// everything old: only the active version and version 1 remain
		n, err = r.Prune(a, 0, libT0.AddDate(1, 0, 0))
		require.NoError(t, err)
		assert.EqualValues(t, 1, n)
		assert.Equal(t, []int{5, 1}, libVersionNumbers(a, t, r, "id1"))
	})

	t.Run("PruneByCountOrAge", func(t *testing.T) {
		r := newRepo(t)
		e, v := libEntry("id1", "A")
		require.NoError(t, r.Insert(a, e, v))
		for n := 2; n <= 5; n++ {
			libAddVersion(a, t, r, "id1", n, libT0.AddDate(0, 0, n))
		}
		// keep 3 would leave 5, 4, 3; old enough takes 3 as well
		n, err := r.Prune(a, 3, libT0.AddDate(0, 0, 4))
		require.NoError(t, err)
		assert.EqualValues(t, 2, n)
		assert.Equal(t, []int{5, 4, 1}, libVersionNumbers(a, t, r, "id1"))
	})

	t.Run("MetaAbsent", func(t *testing.T) {
		r := newRepo(t)
		_, ok, err := r.Meta(a, "k")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("IsolatesTenants", func(t *testing.T) {
		r := newRepo(t)

		// the same id and the same live name exist in both tenants
		ea, va := libEntry("id1", "A")
		require.NoError(t, r.Insert(a, ea, va))
		eb, vb := libEntry("id1", "A")
		eb.Description = "from b"
		require.NoError(t, r.Insert(b, eb, vb))

		got, err := r.Get(a, "id1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "desc", got.Description)

		// a second version in a only
		libAddVersion(a, t, r, "id1", 2, libT0.Add(time.Hour))
		assert.Equal(t, []int{2, 1}, libVersionNumbers(a, t, r, "id1"))
		assert.Equal(t, []int{1}, libVersionNumbers(b, t, r, "id1"))
		gotB, err := r.Get(b, "id1")
		require.NoError(t, err)
		assert.Equal(t, 1, gotB.ActiveVersion)
		assert.Equal(t, "from b", gotB.Description)
		v2, err := r.Version(b, "id1", 2)
		require.NoError(t, err)
		assert.Nil(t, v2)

		// b's version 1 can't be advanced with a's base version
		eb.ActiveVersion = 3
		vb.Version = 3
		assert.ErrorIs(t, r.AddVersion(b, eb, vb, 2), library.ErrConflict)

		// only in a
		ec, vc := libEntry("id2", "C")
		key := "k1"
		ec.BuiltinKey = &key
		require.NoError(t, r.Insert(a, ec, vc))
		listA, err := r.List(a, true)
		require.NoError(t, err)
		assert.Len(t, listA, 2)
		listB, err := r.List(b, true)
		require.NoError(t, err)
		assert.Len(t, listB, 1)
		liveB, err := r.ListLive(b)
		require.NoError(t, err)
		require.Len(t, liveB, 1)
		assert.Equal(t, "from b", liveB[0].Description)
		missing, err := r.Get(b, "id2")
		require.NoError(t, err)
		assert.Nil(t, missing)
		noBuiltin, err := r.ByBuiltinKey(b, "k1")
		require.NoError(t, err)
		assert.Nil(t, noBuiltin)
		noVersions, err := r.Versions(b, "id2")
		require.NoError(t, err)
		assert.Empty(t, noVersions)

		// delete in a leaves b alone
		at := libT0
		require.NoError(t, r.SetDeleted(a, "id1", &at, "u1"))
		stillB, err := r.Get(b, "id1")
		require.NoError(t, err)
		assert.Nil(t, stillB.DeletedAt)
		// b can't delete a's entry
		require.NoError(t, r.SetDeleted(b, "id2", &at, "u1"))
		stillA, err := r.Get(a, "id2")
		require.NoError(t, err)
		assert.Nil(t, stillA.DeletedAt)

		// prune in b leaves a's versions alone
		libAddVersion(a, t, r, "id2", 2, libT0.Add(time.Hour))
		libAddVersion(a, t, r, "id2", 3, libT0.Add(2*time.Hour))
		n, err := r.Prune(b, 1, time.Time{})
		require.NoError(t, err)
		assert.Zero(t, n)
		assert.Equal(t, []int{3, 2, 1}, libVersionNumbers(a, t, r, "id2"))
		n, err = r.Prune(b, 0, libT0.AddDate(1, 0, 0))
		require.NoError(t, err)
		assert.Zero(t, n)
		assert.Equal(t, []int{3, 2, 1}, libVersionNumbers(a, t, r, "id2"))
	})

	t.Run("SyncBuiltinsPerTenant", func(t *testing.T) {
		r := newRepo(t)
		bs := []library.Builtin{libBuiltin("k1", "Uptime", "uptime")}

		created, _, _, err := library.SyncBuiltins(a, r, bs, "1", libT0)
		require.NoError(t, err)
		assert.Equal(t, 1, created)
		none, err := r.ByBuiltinKey(b, "k1")
		require.NoError(t, err)
		assert.Nil(t, none)

		// b gets its own copy under the plain name, without a "(built-in)" suffix
		created, _, _, err = library.SyncBuiltins(b, r, bs, "1", libT0)
		require.NoError(t, err)
		assert.Equal(t, 1, created)
		eb, err := r.ByBuiltinKey(b, "k1")
		require.NoError(t, err)
		require.NotNil(t, eb)
		assert.Equal(t, "Uptime", eb.Name)

		// a second sync changes nothing
		created, updated, _, err := library.SyncBuiltins(a, r, bs, "1", libT0)
		require.NoError(t, err)
		assert.Zero(t, created)
		assert.Zero(t, updated)
	})
}

func libBuiltin(key, name, code string) library.Builtin {
	return library.Builtin{
		Key: key,
		Content: library.Content{
			Name:            name,
			Description:     "built-in " + name,
			Tags:            []string{},
			Implementations: map[string]library.Implementation{library.OSLinux: {Kind: library.KindCommand, Code: code, TimeoutSec: 60}},
		},
	}
}
