package library

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite3"
	bindata "github.com/golang-migrate/migrate/v4/source/go_bindata"
	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/migration/library"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/share/tenant"
)

func ctxFor(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

// preTenantVersion is the last library.db schema version without tenant_id.
const preTenantVersion = 5

// migrateTo opens a file DB and migrates it to the given schema version (up or down).
func migrateTo(t *testing.T, path string, version uint) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("sqlite3", path)
	require.NoError(t, err)
	src, err := bindata.WithInstance(bindata.Resource(library.AssetNames(), library.Asset))
	require.NoError(t, err)
	drv, err := sqlite3.WithInstance(db.DB, &sqlite3.Config{})
	require.NoError(t, err)
	m, err := migrate.NewWithInstance("go-bindata", src, "sqlite3", drv)
	require.NoError(t, err)
	err = m.Migrate(version)
	if err != migrate.ErrNoChange {
		require.NoError(t, err)
	}
	return db
}

func TestStoreRequiresTenant(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	e, v := testEntry("id1", "A")

	assert.ErrorIs(t, s.Insert(ctx, e, v), tenant.ErrMissing)
	assert.ErrorIs(t, s.AddVersion(ctx, e, v, 1), tenant.ErrMissing)
	_, err := s.Get(ctx, "id1")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = s.List(ctx, true)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = s.ListLive(ctx)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = s.Versions(ctx, "id1")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = s.Version(ctx, "id1", 1)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.ErrorIs(t, s.SetDeleted(ctx, "id1", &t0, "u"), tenant.ErrMissing)
	_, err = s.Prune(ctx, 1, time.Time{})
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, _, err = s.Meta(ctx, "k")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = s.ByBuiltinKey(ctx, "k")
	assert.ErrorIs(t, err, tenant.ErrMissing)

	_, err = ImportLegacy(ctx, s.db, s)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, _, _, err = SyncBuiltins(ctx, s, []Builtin{testBuiltin("b1", "B", "echo")}, "1", t0)
	assert.ErrorIs(t, err, tenant.ErrMissing)

	// last: an unscoped write here would hold the write lock
	tx, err := s.db.BeginTxx(context.Background(), nil)
	require.NoError(t, err)
	defer tx.Rollback() //nolint:errcheck
	assert.ErrorIs(t, s.SetMeta(ctx, tx, "k", "v"), tenant.ErrMissing)
}

func TestStoreIsolatesTenants(t *testing.T) {
	s := newTestStore(t)
	a, b := ctxFor("a"), ctxFor("b")

	// the same id and the same live name exist in both tenants
	ea, va := testEntry("id1", "A")
	require.NoError(t, s.Insert(a, ea, va))
	eb, vb := testEntry("id1", "A")
	eb.Description = "from b"
	require.NoError(t, s.Insert(b, eb, vb))

	got, err := s.Get(a, "id1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "desc", got.Description)

	// a second version in a only
	addVersionIn(t, s, a, "id1", 2, t0.Add(time.Hour))
	assert.Equal(t, []int{2, 1}, versionNumbersIn(t, s, a, "id1"))
	assert.Equal(t, []int{1}, versionNumbersIn(t, s, b, "id1"))
	gotB, err := s.Get(b, "id1")
	require.NoError(t, err)
	assert.Equal(t, 1, gotB.ActiveVersion)
	assert.Equal(t, "from b", gotB.Description)
	v2, err := s.Version(b, "id1", 2)
	require.NoError(t, err)
	assert.Nil(t, v2)

	// only in a
	ec, vc := testEntry("id2", "C")
	require.NoError(t, s.Insert(a, ec, vc))
	listA, err := s.List(a, true)
	require.NoError(t, err)
	assert.Len(t, listA, 2)
	listB, err := s.List(b, true)
	require.NoError(t, err)
	assert.Len(t, listB, 1)
	liveB, err := s.ListLive(b)
	require.NoError(t, err)
	require.Len(t, liveB, 1)
	assert.Equal(t, "from b", liveB[0].Description)
	missing, err := s.Get(b, "id2")
	require.NoError(t, err)
	assert.Nil(t, missing)

	// delete in a leaves b alone
	require.NoError(t, s.SetDeleted(a, "id1", &t0, "u1"))
	stillB, err := s.Get(b, "id1")
	require.NoError(t, err)
	assert.Nil(t, stillB.DeletedAt)
	// b can't delete a's entry
	require.NoError(t, s.SetDeleted(b, "id2", &t0, "u1"))
	stillA, err := s.Get(a, "id2")
	require.NoError(t, err)
	assert.Nil(t, stillA.DeletedAt)

	// prune in b leaves a's versions alone
	addVersionIn(t, s, a, "id2", 2, t0.Add(time.Hour))
	addVersionIn(t, s, a, "id2", 3, t0.Add(2*time.Hour))
	n, err := s.Prune(b, 1, time.Time{})
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, []int{3, 2, 1}, versionNumbersIn(t, s, a, "id2"))

	// meta per tenant
	tx, err := s.db.BeginTxx(a, nil)
	require.NoError(t, err)
	require.NoError(t, s.SetMeta(a, tx, "k", "va"))
	require.NoError(t, tx.Commit())
	_, ok, err := s.Meta(b, "k")
	require.NoError(t, err)
	assert.False(t, ok)
	val, ok, err := s.Meta(a, "k")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "va", val)
}

func TestSyncBuiltinsPerTenant(t *testing.T) {
	s := newTestStore(t)
	a, b := ctxFor("a"), ctxFor("b")
	bs := []Builtin{testBuiltin("k1", "Uptime", "uptime")}

	created, _, _, err := SyncBuiltins(a, s, bs, "1", t0)
	require.NoError(t, err)
	assert.Equal(t, 1, created)
	none, err := s.ByBuiltinKey(b, "k1")
	require.NoError(t, err)
	assert.Nil(t, none)

	// b gets its own copy under the plain name, without a "(built-in)" suffix
	created, _, _, err = SyncBuiltins(b, s, bs, "1", t0)
	require.NoError(t, err)
	assert.Equal(t, 1, created)
	eb, err := s.ByBuiltinKey(b, "k1")
	require.NoError(t, err)
	require.NotNil(t, eb)
	assert.Equal(t, "Uptime", eb.Name)
}

func TestImportLegacyIsDefaultOnly(t *testing.T) {
	db, s := newLegacyDB(t)
	seedScript(t, db, "s1", "one", "", false, "[]")

	n, err := ImportLegacy(ctxFor("a"), db, s)
	require.NoError(t, err)
	assert.Zero(t, n)
	list, err := s.List(ctxFor("a"), true)
	require.NoError(t, err)
	assert.Empty(t, list)
	_, done, err := s.Meta(ctxFor("a"), legacyImportKey)
	require.NoError(t, err)
	assert.False(t, done)

	n, err = ImportLegacy(ctxFor(tenant.Default), db, s)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestPruneTaskRequiresTenant(t *testing.T) {
	m, _ := newTestManager(t)
	assert.ErrorIs(t, NewPruneTask(m, 1, 0, nil).Run(context.Background()), tenant.ErrMissing)
}

func TestLibraryJSONHasNoTenant(t *testing.T) {
	e, v := testEntry("id1", "A")
	for _, x := range []interface{}{e, v} {
		out, err := json.Marshal(x)
		require.NoError(t, err)
		assert.NotContains(t, string(out), "tenant")
	}
}

func TestUpgradeKeepsLibraryInDefaultTenant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.db")
	old := migrateTo(t, path, preTenantVersion)
	ins := func(q string, args ...interface{}) {
		_, err := old.Exec(q, args...)
		require.NoError(t, err)
	}
	content := func(name, code string) string {
		c := Content{Name: name, Tags: []string{}, Implementations: map[string]Implementation{OSLinux: {Kind: KindCommand, Code: code, TimeoutSec: 60}}}
		out, err := json.Marshal(c)
		require.NoError(t, err)
		return string(out)
	}
	const entryCols = "INSERT INTO `entries` (`id`, `name`, `description`, `tags`, `os`, `active_version`, `builtin_key`, `created_at`, `created_by`, `updated_at`, `updated_by`, `deleted_at`, `deleted_by`) VALUES (?, ?, '', '[]', '[\"linux\"]', ?, ?, ?, 'alice', ?, 'alice', ?, ?)"
	const versionCols = "INSERT INTO `entry_versions` (`entry_id`, `version`, `created_at`, `created_by`, `message`, `changed`, `content`) VALUES (?, ?, ?, 'alice', 'm', '[]', ?)"
	// a user entry with three versions
	ins(entryCols, "user1", "Disk", 3, nil, t0, t0, nil, "")
	for n := 1; n <= 3; n++ {
		ins(versionCols, "user1", n, t0.Add(time.Duration(n)*time.Hour), content("Disk", fmt.Sprintf("df %d", n)))
	}
	// a built-in, as shipped
	bi := testBuiltin("k1", "Uptime", "uptime")
	biContent, err := json.Marshal(bi.Content)
	require.NoError(t, err)
	ins("INSERT INTO `entries` (`id`, `name`, `description`, `tags`, `os`, `active_version`, `builtin_key`, `created_at`, `created_by`, `updated_at`, `updated_by`) VALUES ('bi1', 'Uptime', 'd', '[\"procy\",\"k1\"]', '[\"linux\"]', 1, 'k1', ?, 'procy', ?, 'procy')", t0, t0)
	ins(versionCols, "bi1", 1, t0, string(biContent))
	// a soft-deleted entry sharing a live entry's name
	ins(entryCols, "del1", "Disk", 1, nil, t0, t0, t0, "bob")
	ins(versionCols, "del1", 1, t0, content("Disk", "df old"))
	// the legacy import already ran
	ins("INSERT INTO `library_meta` (`key`, `value`) VALUES ('legacy_import', '0')")
	require.NoError(t, old.Close())

	db, err := sqlite.New(path, library.AssetNames(), library.Asset, sqlite.DataSourceOptions{})
	require.NoError(t, err)
	defer db.Close()
	s := NewStore(db)
	ctx := ctxFor(tenant.Default)

	all, err := s.List(ctx, true)
	require.NoError(t, err)
	assert.Len(t, all, 3)
	live, err := s.ListLive(ctx)
	require.NoError(t, err)
	assert.Len(t, live, 2)

	u, err := s.Get(ctx, "user1")
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.Equal(t, "df 3", u.Implementations[OSLinux].Code)
	assert.Equal(t, []int{3, 2, 1}, versionNumbersIn(t, s, ctx, "user1"))

	d, err := s.Get(ctx, "del1")
	require.NoError(t, err)
	require.NotNil(t, d.DeletedAt)
	assert.Equal(t, "bob", d.DeletedBy)

	// the built-in is found by its key, so a sync doesn't duplicate it
	biEntry, err := s.ByBuiltinKey(ctx, "k1")
	require.NoError(t, err)
	require.NotNil(t, biEntry)
	assert.Equal(t, "bi1", biEntry.ID)
	created, updated, _, err := SyncBuiltins(ctx, s, []Builtin{bi}, "1", t0)
	require.NoError(t, err)
	assert.Zero(t, created)
	assert.Zero(t, updated)

	// the legacy import does not run again
	n, err := ImportLegacy(ctx, db, s)
	require.NoError(t, err)
	assert.Zero(t, n)

	// the live-name rule still holds in the default tenant
	e, v := testEntry("new1", "Disk")
	assert.ErrorIs(t, s.Insert(ctx, e, v), ErrNameTaken)
}

func TestDowngradeKeepsOnlyDefaultLibrary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.db")
	db, err := sqlite.New(path, library.AssetNames(), library.Asset, sqlite.DataSourceOptions{})
	require.NoError(t, err)
	s := NewStore(db)
	for _, tid := range []tenant.ID{tenant.Default, "a"} {
		ctx := ctxFor(tid)
		e, v := testEntry("id1", "A")
		e.Description = string(tid)
		require.NoError(t, s.Insert(ctx, e, v))
		addVersionIn(t, s, ctx, "id1", 2, t0.Add(time.Hour))
		tx, err := db.BeginTxx(ctx, nil)
		require.NoError(t, err)
		require.NoError(t, s.SetMeta(ctx, tx, "k", string(tid)))
		require.NoError(t, tx.Commit())
	}
	require.NoError(t, db.Close())

	down := migrateTo(t, path, preTenantVersion)
	defer down.Close()
	var ids []string
	require.NoError(t, down.Select(&ids, "SELECT `id` FROM `entries`"))
	assert.Equal(t, []string{"id1"}, ids)
	var versions int
	require.NoError(t, down.Get(&versions, "SELECT COUNT(*) FROM `entry_versions`"))
	assert.Equal(t, 2, versions)
	var meta string
	require.NoError(t, down.Get(&meta, "SELECT `value` FROM `library_meta` WHERE `key` = 'k'"))
	assert.Equal(t, "default", meta)
	for _, table := range []string{"entries", "entry_versions", "library_meta"} {
		var n int
		require.NoError(t, down.Get(&n, "SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'tenant_id'", table))
		assert.Zero(t, n, table)
	}
	// the old live-name index is back
	_, err = down.Exec("INSERT INTO `entries` (`id`, `name`, `active_version`, `created_at`, `created_by`, `updated_at`, `updated_by`) VALUES ('id2', 'A', 1, ?, 'u', ?, 'u')", t0, t0)
	assert.ErrorContains(t, err, "UNIQUE constraint failed")
}
