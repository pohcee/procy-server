package importer

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clientgroupsmigration "github.com/openrport/openrport/db/migration/client_groups"
	clientsmigration "github.com/openrport/openrport/db/migration/clients"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/cgroups"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/server/clients/storedtunnels"
	"github.com/openrport/openrport/server/repotest"
	pgclientgroups "github.com/openrport/openrport/server/storage/postgres/clientgroups"
	pgclients "github.com/openrport/openrport/server/storage/postgres/clients"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	pgstoredtunnels "github.com/openrport/openrport/server/storage/postgres/storedtunnels"
	"github.com/openrport/openrport/share/types"
)

func TestImportClientsTunnelsGroups(t *testing.T) {
	pg := pgtest.New(t)
	d := newDataDir(t)
	disconnected := time.Date(2026, 3, 4, 5, 6, 7, 890000000, time.UTC)
	var wantClients []*clientdata.Client
	var wantTunnels []*storedtunnels.StoredTunnel
	d.with("clients.db", clientsmigration.FS, func(db *sqlx.DB) {
		s := clients.NewSQLiteStore(db, nil)
		require.NoError(t, s.Save(ctxDefault, repotest.StoreClient("c1", &disconnected)))
		require.NoError(t, s.Save(ctxDefault, repotest.StoreClient("c2", nil)))
		var err error
		wantClients, err = s.GetAll(ctxDefault, testLog)
		require.NoError(t, err)

		st := storedtunnels.NewSQLiteProvider(db)
		port, scheme, opts := 22, "ssh", types.JSONString(`{"idle_timeout_minutes":5}`)
		require.NoError(t, st.Insert(ctxDefault, &storedtunnels.StoredTunnel{
			ID: "t1", ClientID: "c1", CreatedAt: disconnected, Name: "ssh", Scheme: &scheme, RemotePort: &port, FurtherOptions: &opts,
		}))
		// a tunnel whose client is gone: SQLite never enforced the reference
		require.NoError(t, st.Insert(ctxDefault, &storedtunnels.StoredTunnel{ID: "t2", ClientID: "gone", CreatedAt: disconnected, Name: "orphan"}))
		wantTunnels, err = st.List(ctxDefault, "c1", nil)
		require.NoError(t, err)
	})
	var wantGroups []*cgroups.ClientGroup
	d.with("client_groups.db", clientgroupsmigration.FS, func(db *sqlx.DB) {
		s, err := cgroups.NewSqliteProvider(db)
		require.NoError(t, err)
		ids := cgroups.ParamValues{"c1", "c2*"}
		require.NoError(t, s.Create(ctxDefault, &cgroups.ClientGroup{
			ID: "g1", Description: "group one", Params: &cgroups.ClientParams{ClientID: &ids}, AllowedUserGroups: []string{"ops"},
		}))
		wantGroups, err = s.GetAll(ctxDefault)
		require.NoError(t, err)
	})

	rep := run(t, pg, Options{From: d.dir})
	want(t, rep, entClients, 2, 2, 0, 0)
	want(t, rep, entStoredTunnels, 2, 1, 0, 1)
	want(t, rep, entClientGroups, 1, 1, 0, 0)
	assert.True(t, hasWarning(rep, "stored tunnels: 1 of clients that don't exist"), rep.Warnings)

	gotClients, err := pgclients.New(pg.App, nil).GetAll(ctxDefault, testLog)
	require.NoError(t, err)
	require.Len(t, gotClients, 2)
	for i := range wantClients {
		repotest.AssertSameClient(t, wantClients[i], gotClients[i])
	}
	gotTunnels, err := pgstoredtunnels.New(pg.App).List(ctxDefault, "c1", nil)
	require.NoError(t, err)
	sameJSON(t, wantTunnels, gotTunnels)
	gotGroups, err := pgclientgroups.New(pg.App).GetAll(ctxDefault)
	require.NoError(t, err)
	sameJSON(t, wantGroups, gotGroups)
}

func hasWarning(rep *Report, part string) bool {
	for _, w := range rep.Warnings {
		if strings.Contains(w, part) {
			return true
		}
	}
	return false
}

// A server that stopped without checkpointing leaves its last writes in the -wal file: they are read from a copy
// with the WAL replayed, and the data dir is left as it was.
func TestImportReplaysPendingWAL(t *testing.T) {
	pg := pgtest.New(t)
	live := newDataDir(t)
	db, err := sqlite.New(live.path("clients.db"), clientsmigration.FS, sqlite.DataSourceOptions{WALEnabled: true})
	require.NoError(t, err)
	require.NoError(t, clients.NewSQLiteStore(db, nil).Save(ctxDefault, repotest.StoreClient("c1", nil)))
	// copy the files while the connection is open: the row is only in the WAL
	stopped := newDataDir(t)
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(live.path("clients.db") + suffix)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(stopped.path("clients.db")+suffix, b, 0o600)) //nolint:gosec // G703: a test temp dir
	}
	require.NoError(t, db.Close())
	walInfo, err := os.Stat(stopped.path("clients.db-wal"))
	require.NoError(t, err)
	require.Positive(t, walInfo.Size())
	before := fileSum(t, stopped.path("clients.db"))

	rep := run(t, pg, Options{From: stopped.dir})
	want(t, rep, entClients, 1, 1, 0, 0)
	assert.True(t, hasWarning(rep, "clients.db-wal"), rep.Warnings)
	assert.Equal(t, before, fileSum(t, stopped.path("clients.db")))
	after, err := os.Stat(stopped.path("clients.db-wal"))
	require.NoError(t, err)
	assert.Equal(t, walInfo.Size(), after.Size())
}
