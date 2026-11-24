package library

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/migration/library"
	"github.com/openrport/openrport/db/sqlite"
)

func newLegacyDB(t *testing.T) (*sqlx.DB, *Store) {
	t.Helper()
	db, err := sqlite.New(filepath.Join(t.TempDir(), "library.db"), library.FS, sqlite.DataSourceOptions{WALEnabled: false})
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, NewStore(db)
}

var (
	created = time.Date(2025, 3, 1, 8, 0, 0, 0, time.UTC)
	updated = time.Date(2025, 6, 2, 9, 30, 0, 0, time.UTC)
)

func seedScript(t *testing.T, db *sqlx.DB, id, name, interpreter string, sudo bool, tags string) {
	t.Helper()
	_, err := db.Exec("INSERT INTO `scripts` (`id`, `name`, `created_at`, `created_by`, `updated_at`, `updated_by`, `interpreter`, `is_sudo`, `cwd`, `script`, `tags`, `timeout_sec`) VALUES (?, ?, ?, 'alice', ?, 'bob', ?, ?, '/srv', 'echo hi', ?, 45)",
		id, name, created, updated, interpreter, sudo, tags)
	require.NoError(t, err)
}

func seedCommand(t *testing.T, db *sqlx.DB, id, name, cmd string) {
	t.Helper()
	_, err := db.Exec("INSERT INTO `commands` (`id`, `name`, `created_at`, `created_by`, `updated_at`, `updated_by`, `cmd`, `tags`, `timeout_sec`) VALUES (?, ?, ?, 'carol', ?, 'dave', ?, '[\"x\"]', 20)",
		id, name, created, updated, cmd)
	require.NoError(t, err)
}

func TestImportLegacyMapsOSByInterpreter(t *testing.T) {
	db, s := newLegacyDB(t)
	seedScript(t, db, "s1", "ps", "powershell", false, `["w"]`)
	seedScript(t, db, "s2", "cmdscript", "cmd", false, "[]")
	seedScript(t, db, "s3", "bash", "/bin/bash", true, `["b","a"]`)
	seedScript(t, db, "s4", "nointerp", "", false, "[]")
	seedCommand(t, db, "c1", "ls", "ls -la")

	n, err := ImportLegacy(testCtx, db, s)
	require.NoError(t, err)
	assert.Equal(t, 5, n)

	get := func(id string) *Entry {
		e, err := s.Get(testCtx, id)
		require.NoError(t, err)
		require.NotNil(t, e)
		return e
	}
	assert.Equal(t, []string{OSWindows}, get("s1").OS)
	assert.Equal(t, []string{OSWindows}, get("s2").OS)
	assert.Equal(t, []string{OSLinux}, get("s4").OS)

	e := get("s3")
	assert.Equal(t, []string{OSLinux}, e.OS)
	assert.Equal(t, []string{"a", "b"}, e.Tags)
	assert.Equal(t, Implementation{Kind: KindScript, Code: "echo hi", Interpreter: "/bin/bash", IsSudo: true, Cwd: "/srv", TimeoutSec: 45}, e.Implementations[OSLinux])

	c := get("c1")
	assert.Equal(t, []string{OSLinux}, c.OS)
	assert.Equal(t, []string{"x"}, c.Tags)
	assert.Equal(t, Implementation{Kind: KindCommand, Code: "ls -la", TimeoutSec: 20}, c.Implementations[OSLinux])
}

func TestImportLegacyKeepsAuthorship(t *testing.T) {
	db, s := newLegacyDB(t)
	seedScript(t, db, "s1", "one", "", false, "[]")
	seedCommand(t, db, "c1", "two", "uptime")
	_, err := ImportLegacy(testCtx, db, s)
	require.NoError(t, err)

	e, err := s.Get(testCtx, "s1")
	require.NoError(t, err)
	assert.True(t, created.Equal(e.CreatedAt))
	assert.Equal(t, "alice", e.CreatedBy)
	assert.True(t, updated.Equal(e.UpdatedAt))
	assert.Equal(t, "bob", e.UpdatedBy)
	assert.Equal(t, 1, e.ActiveVersion)

	v, err := s.Version(testCtx, "s1", 1)
	require.NoError(t, err)
	assert.True(t, created.Equal(v.CreatedAt))
	assert.Equal(t, "alice", v.CreatedBy)
	assert.Equal(t, "Imported from the script library", v.Message)

	v, err = s.Version(testCtx, "c1", 1)
	require.NoError(t, err)
	assert.Equal(t, "carol", v.CreatedBy)
	assert.Equal(t, "Imported from the command library", v.Message)
}

func TestImportLegacyKeepsNonAbsoluteCommand(t *testing.T) {
	db, s := newLegacyDB(t)
	seedCommand(t, db, "c1", "weird", "echo a | grep a")
	_, err := ImportLegacy(testCtx, db, s)
	require.NoError(t, err)
	e, err := s.Get(testCtx, "c1")
	require.NoError(t, err)
	assert.Equal(t, KindCommand, e.Implementations[OSLinux].Kind)
}

func TestImportLegacyNameClash(t *testing.T) {
	db, s := newLegacyDB(t)
	seedScript(t, db, "same", "restart rport", "", false, "[]")
	seedCommand(t, db, "same", "restart rport", "systemctl restart rport")

	n, err := ImportLegacy(testCtx, db, s)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	entries, err := s.List(testCtx, false)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "restart rport", entries[0].Name)
	assert.Equal(t, "restart rport (command)", entries[1].Name)
	assert.Equal(t, "same", entries[0].ID)
	assert.NotEqual(t, "same", entries[1].ID) // id collision gets a fresh UUID
}

func TestImportLegacyRunsOnce(t *testing.T) {
	db, s := newLegacyDB(t)
	seedScript(t, db, "s1", "one", "", false, "[]")
	n, err := ImportLegacy(testCtx, db, s)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	seedCommand(t, db, "c1", "late", "ls")
	n, err = ImportLegacy(testCtx, db, s)
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	entries, err := s.List(testCtx, true)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestImportLegacyEmptyStillMarksDone(t *testing.T) {
	db, s := newLegacyDB(t)
	n, err := ImportLegacy(testCtx, db, s)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	v, ok, err := s.Meta(testCtx, "legacy_import")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "0", v)
}

func TestImportLegacyNameClashWhitespaceAndScripts(t *testing.T) {
	db, s := newLegacyDB(t)
	seedScript(t, db, "s1", "a", "", false, "[]")
	seedScript(t, db, "s2", "a ", "", false, "[]")
	seedScript(t, db, "s3", " a", "", false, "[]")
	seedCommand(t, db, "c1", "a", "ls")
	seedCommand(t, db, "c2", "a ", "ls")
	seedCommand(t, db, "c3", "a (command)", "ls")

	n, err := ImportLegacy(testCtx, db, s)
	require.NoError(t, err)
	assert.Equal(t, 6, n)

	names := map[string]string{}
	for _, id := range []string{"s1", "s2", "s3", "c1", "c2", "c3"} {
		e, err := s.Get(testCtx, id)
		require.NoError(t, err)
		require.NotNil(t, e)
		names[id] = e.Name
	}
	assert.ElementsMatch(t, []string{"a", "a (2)", "a (3)"}, []string{names["s1"], names["s2"], names["s3"]})
	assert.Equal(t, "a (command)", names["c1"])
	assert.Equal(t, "a (command 2)", names["c2"])
	assert.NotEmpty(t, names["c3"])
	seen := map[string]bool{}
	for _, nm := range names {
		assert.False(t, seen[nm], "duplicate name %q", nm)
		seen[nm] = true
	}
}

func TestImportLegacyMixedCaseInterpreterAndNulls(t *testing.T) {
	db, s := newLegacyDB(t)
	seedScript(t, db, "s1", "ps", "PowerShell", false, "[]")
	_, err := db.Exec("INSERT INTO `scripts` (`id`, `name`, `created_at`, `created_by`, `updated_at`, `updated_by`, `interpreter`, `is_sudo`, `cwd`, `script`, `tags`, `timeout_sec`) VALUES ('s2', 'nulls', ?, 'a', ?, 'b', NULL, 0, NULL, 'echo', '[]', 0)", created, updated)
	require.NoError(t, err)

	n, err := ImportLegacy(testCtx, db, s)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	e, err := s.Get(testCtx, "s1")
	require.NoError(t, err)
	assert.Equal(t, []string{OSWindows}, e.OS)
	e, err = s.Get(testCtx, "s2")
	require.NoError(t, err)
	assert.Equal(t, []string{OSLinux}, e.OS)
	impl := e.Implementations[OSLinux]
	assert.Equal(t, "", impl.Interpreter)
	assert.Equal(t, "", impl.Cwd)
	assert.False(t, impl.IsSudo)
}
