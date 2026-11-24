package migration

import (
	"io/fs"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/migration/api_sessions"
	"github.com/openrport/openrport/db/migration/api_token"
	"github.com/openrport/openrport/db/migration/auditlog"
	"github.com/openrport/openrport/db/migration/client_groups"
	"github.com/openrport/openrport/db/migration/clients"
	"github.com/openrport/openrport/db/migration/dummy"
	"github.com/openrport/openrport/db/migration/jobs"
	"github.com/openrport/openrport/db/migration/library"
	"github.com/openrport/openrport/db/migration/monitoring"
	"github.com/openrport/openrport/db/migration/vaults"
	"github.com/openrport/openrport/db/sqlite"
	notifications "github.com/openrport/openrport/server/notifications/repository/sqlite"
)

// Transitional: proves the embedded SQL files are exactly the go-bindata assets
// until the generated bindata files are removed.

type bindataSet struct {
	name       string
	fs         fs.FS
	assetNames func() []string
	asset      func(string) ([]byte, error)
}

func bindataSets() []bindataSet {
	return []bindataSet{
		{"clients", clients.FS, clients.AssetNames, clients.Asset},
		{"jobs", jobs.FS, jobs.AssetNames, jobs.Asset},
		{"client_groups", client_groups.FS, client_groups.AssetNames, client_groups.Asset},
		{"monitoring", monitoring.FS, monitoring.AssetNames, monitoring.Asset},
		{"notifications", notifications.Migrations, notifications.AssetNames, notifications.Asset},
		{"library", library.FS, library.AssetNames, library.Asset},
		{"api_token", api_token.FS, api_token.AssetNames, api_token.Asset},
		{"api_sessions", api_sessions.FS, api_sessions.AssetNames, api_sessions.Asset},
		{"vaults", vaults.FS, vaults.AssetNames, vaults.Asset},
		{"auditlog", auditlog.FS, auditlog.AssetNames, auditlog.Asset},
		{"dummy", dummy.FS, dummy.AssetNames, dummy.Asset},
	}
}

func TestAllListsEveryRealSet(t *testing.T) {
	var got []string
	for _, s := range All() {
		got = append(got, s.Name)
	}
	var want []string
	for _, s := range bindataSets() {
		if s.name != "dummy" {
			want = append(want, s.name)
		}
	}
	assert.ElementsMatch(t, want, got)
}

func TestEmbeddedFilesEqualBindata(t *testing.T) {
	for _, s := range bindataSets() {
		t.Run(s.name, func(t *testing.T) {
			want := append([]string(nil), s.assetNames()...)
			sort.Strings(want)

			entries, err := fs.ReadDir(s.fs, ".")
			require.NoError(t, err)
			var got []string
			for _, e := range entries {
				require.False(t, e.IsDir(), e.Name())
				got = append(got, e.Name())
			}
			sort.Strings(got)
			require.Equal(t, want, got)

			// dummy is test-only: its .sql was corrected (34d7facb) without
			// regenerating bindata, so only its names are compared.
			if s.name == "dummy" {
				return
			}
			for _, name := range want {
				wantBytes, err := s.asset(name)
				require.NoError(t, err)
				gotBytes, err := fs.ReadFile(s.fs, name)
				require.NoError(t, err)
				assert.Equal(t, wantBytes, gotBytes, name)
			}
		})
	}
}

func TestBindataDatabaseRecognisedByEmbed(t *testing.T) {
	for _, s := range bindataSets() {
		t.Run(s.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), s.name+".db")
			db, err := sqlite.New(path, s.assetNames(), s.asset, sqlite.DataSourceOptions{})
			require.NoError(t, err)
			var wantVersion int
			var wantDirty bool
			require.NoError(t, db.QueryRow("SELECT version, dirty FROM schema_migrations").Scan(&wantVersion, &wantDirty))
			require.False(t, wantDirty)
			require.NoError(t, db.Close())

			db, err = sqlite.Open(path, sqlite.DataSourceOptions{})
			require.NoError(t, err)
			defer db.Close()
			require.NoError(t, sqlite.CheckVersion(db, s.fs))
			require.NoError(t, sqlite.Migrate(db, s.fs))

			var gotVersion, rows int
			var gotDirty bool
			require.NoError(t, db.QueryRow("SELECT version, dirty FROM schema_migrations").Scan(&gotVersion, &gotDirty))
			require.NoError(t, db.Get(&rows, "SELECT COUNT(*) FROM schema_migrations"))
			assert.Equal(t, wantVersion, gotVersion)
			assert.False(t, gotDirty)
			assert.Equal(t, 1, rows)

			db2, err := sqlite.NewFS(path, s.fs, sqlite.DataSourceOptions{})
			require.NoError(t, err)
			require.NoError(t, db2.Close())
		})
	}
}
