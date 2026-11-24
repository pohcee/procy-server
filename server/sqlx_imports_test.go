package chserver

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// sqlxAllowed reports whether the non-test Go file at rel (slash-separated, relative to the module root) may import
// sqlx: SQLite implementations, the central SQLite wiring and the [database] auth providers.
func sqlxAllowed(rel string) bool {
	if strings.HasPrefix(filepath.Base(rel), "sqlite") {
		return true
	}
	for _, prefix := range []string{
		"db/sqlite/",
		"server/notifications/repository/sqlite/",
		"cmd/rportd/usercli/",
		"share/test/",
	} {
		if strings.HasPrefix(rel, prefix) {
			return true
		}
	}
	switch rel {
	case "server/storage_sqlite.go",
		"server/api/users/database.go",
		"server/api/users/provider_config.go",
		"server/clientsauth/database.go",
		"server/clientsauth/clientauth_mocks.go":
		return true
	}
	return false
}

// TestSQLXOnlyInSQLiteFiles keeps SQL in the SQLite implementations: services and wiring reach storage through the
// repository interfaces only.
func TestSQLXOnlyInSQLiteFiles(t *testing.T) {
	root, err := filepath.Abs("..")
	require.NoError(t, err)

	var offenders []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if p != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if path == "github.com/jmoiron/sqlx" && !sqlxAllowed(rel) {
				offenders = append(offenders, rel)
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, offenders, "these files import sqlx; move the SQL into a sqlite*.go implementation")
}
