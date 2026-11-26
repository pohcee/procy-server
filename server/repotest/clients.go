package repotest

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/clients"
	"github.com/pohcee/procy-server/server/clients/clientdata"
	"github.com/pohcee/procy-server/server/clients/clienttunnel"
	"github.com/pohcee/procy-server/share/clientconfig"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/models"
	"github.com/pohcee/procy-server/share/tenant"
)

// ClientStoreFactory returns an empty client store in which TenantA and TenantB exist. keepDisconnectedClients
// has the meaning of clients.NewSQLiteStore's parameter: nil keeps disconnected clients forever, otherwise
// clients disconnected for longer are obsolete (left out of GetAll and removed by DeleteObsolete). As in the
// SQLite store, the window is measured back from clientdata.Now (which the suite moves in one case).
type ClientStoreFactory func(t *testing.T, keepDisconnectedClients *time.Duration) clients.ClientStore

var testLogger = logger.NewLogger("repotest", logger.LogOutput{}, logger.LogLevelError)

// ClientStore checks a clients.ClientStore.
func ClientStore(t *testing.T, newRepo ClientStoreFactory) {
	keep := time.Hour
	a, b := Ctx(TenantA), Ctx(TenantB)

	t.Run("requires tenant", func(t *testing.T) {
		s := newRepo(t, &keep)
		ctx := context.Background()
		_, err := s.GetAll(ctx, testLogger)
		assert.ErrorIs(t, err, tenant.ErrMissing)
		assert.ErrorIs(t, s.Save(ctx, StoreClient("c1", nil)), tenant.ErrMissing)
		assert.ErrorIs(t, s.DeleteObsolete(ctx, testLogger), tenant.ErrMissing)
		assert.ErrorIs(t, s.Delete(ctx, "c1", testLogger), tenant.ErrMissing)
	})

	t.Run("empty", func(t *testing.T) {
		s := newRepo(t, &keep)
		all, err := s.GetAll(a, testLogger)
		require.NoError(t, err)
		assert.Empty(t, all)
	})

	t.Run("round trip of every stored field", func(t *testing.T) {
		s := newRepo(t, nil)
		connected := fullClient("connected", nil)
		at := clientdata.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
		disconnected := fullClient("disconnected", &at)
		require.NoError(t, s.Save(a, connected))
		require.NoError(t, s.Save(a, disconnected))

		all, err := s.GetAll(a, testLogger)
		require.NoError(t, err)
		require.Len(t, all, 2)
		sortClients(all)
		AssertSameClient(t, connected, all[0])
		AssertSameClient(t, disconnected, all[1])
		assert.True(t, all[0].IsConnected())
		assert.False(t, all[1].IsConnected())
		assert.Same(t, testLogger, all[0].Logger)
	})

	t.Run("save replaces a client", func(t *testing.T) {
		s := newRepo(t, nil)
		c := StoreClient("c1", nil)
		require.NoError(t, s.Save(a, c))
		at := clientdata.Now().UTC().Add(-time.Minute).Truncate(time.Second)
		c.DisconnectedAt = &at
		c.Name = "renamed"
		c.ClientAuthID = "other-auth"
		c.Tags = []string{"x"}
		require.NoError(t, s.Save(a, c))

		all, err := s.GetAll(a, testLogger)
		require.NoError(t, err)
		require.Len(t, all, 1)
		AssertSameClient(t, c, all[0])

		// and back to connected
		c.DisconnectedAt = nil
		require.NoError(t, s.Save(a, c))
		all, err = s.GetAll(a, testLogger)
		require.NoError(t, err)
		require.Len(t, all, 1)
		assert.True(t, all[0].IsConnected())
	})

	t.Run("obsolete clients are left out and deleted", func(t *testing.T) {
		s := newRepo(t, &keep)
		now := clientdata.Now().UTC().Truncate(time.Second)
		recent, old := now.Add(-5*time.Minute), now.Add(-keep-time.Hour)
		require.NoError(t, s.Save(a, StoreClient("connected", nil)))
		require.NoError(t, s.Save(a, StoreClient("recent", &recent)))
		require.NoError(t, s.Save(a, StoreClient("obsolete", &old)))

		assert.ElementsMatch(t, []string{"connected", "recent"}, clientIDs(a, t, s))

		// a store keeping disconnected clients forever sees and keeps all of them
		forever := newRepo(t, nil)
		require.NoError(t, forever.Save(a, StoreClient("connected", nil)))
		require.NoError(t, forever.Save(a, StoreClient("obsolete", &old)))
		require.NoError(t, forever.DeleteObsolete(a, testLogger))
		assert.ElementsMatch(t, []string{"connected", "obsolete"}, clientIDs(a, t, forever))

		require.NoError(t, s.DeleteObsolete(a, testLogger))
		assert.ElementsMatch(t, []string{"connected", "recent"}, clientIDs(a, t, s))
	})

	t.Run("delete", func(t *testing.T) {
		s := newRepo(t, nil)
		require.NoError(t, s.Save(a, StoreClient("c1", nil)))
		require.NoError(t, s.Save(a, StoreClient("c2", nil)))
		require.NoError(t, s.Delete(a, "c1", testLogger))
		require.NoError(t, s.Delete(a, "unknown", testLogger))
		assert.Equal(t, []string{"c2"}, clientIDs(a, t, s))
	})

	t.Run("isolates tenants", func(t *testing.T) {
		s := newRepo(t, &keep)
		ca := StoreClient("same-id", nil)
		ca.ClientAuthID = "from a"
		cb := StoreClient("same-id", nil)
		cb.ClientAuthID = "from b"
		require.NoError(t, s.Save(a, ca))
		require.NoError(t, s.Save(b, cb))

		all, err := s.GetAll(b, testLogger)
		require.NoError(t, err)
		require.Len(t, all, 1)
		assert.Equal(t, "from b", all[0].GetClientAuthID())

		// saving in a doesn't change b's client
		ca.Name = "a renamed"
		require.NoError(t, s.Save(a, ca))
		all, err = s.GetAll(b, testLogger)
		require.NoError(t, err)
		require.Len(t, all, 1)
		AssertSameClient(t, cb, all[0])

		// obsolete clients are deleted only in the tenant asked
		old := clientdata.Now().UTC().Add(-keep - time.Hour).Truncate(time.Second)
		require.NoError(t, s.Save(a, StoreClient("obsolete", &old)))
		require.NoError(t, s.Save(b, StoreClient("obsolete", &old)))
		require.NoError(t, s.DeleteObsolete(a, testLogger))
		forever := clientIDsOfAll(b, t, s, keep)
		assert.Contains(t, forever, "obsolete")

		require.NoError(t, s.Delete(a, "same-id", testLogger))
		assert.Empty(t, clientIDs(a, t, s))
		assert.Equal(t, []string{"same-id"}, clientIDs(b, t, s))
	})
}

// clientIDsOfAll returns the ids of all the tenant's clients, obsolete ones included: GetAll leaves out clients
// disconnected for longer than keep before clientdata.Now, so clientdata.Now is moved back while it runs.
func clientIDsOfAll(ctx context.Context, t *testing.T, s clients.ClientStore, keep time.Duration) []string {
	t.Helper()
	// GetAll hides obsolete clients; move clientdata.Now back so that they count as recent.
	orig := clientdata.Now
	base := orig()
	clientdata.Now = func() time.Time { return base.Add(-keep - 2*time.Hour) }
	defer func() { clientdata.Now = orig }()
	return clientIDs(ctx, t, s)
}

func clientIDs(ctx context.Context, t *testing.T, s clients.ClientStore) []string {
	t.Helper()
	all, err := s.GetAll(ctx, testLogger)
	require.NoError(t, err)
	ids := make([]string, 0, len(all))
	for _, c := range all {
		ids = append(ids, c.GetID())
	}
	sort.Strings(ids)
	return ids
}

func sortClients(cs []*clientdata.Client) {
	sort.Slice(cs, func(i, j int) bool { return cs[i].GetID() < cs[j].GetID() })
}

// StoreClient returns a client with a few fields set, disconnected at the given time (nil: connected).
func StoreClient(id string, disconnectedAt *time.Time) *clientdata.Client {
	return &clientdata.Client{
		ID:             id,
		ClientAuthID:   "auth-" + id,
		Name:           "name of " + id,
		Hostname:       id + ".example.com",
		DisconnectedAt: disconnectedAt,
		Logger:         testLogger,
	}
}

func fullClient(id string, disconnectedAt *time.Time) *clientdata.Client {
	scheme, acl := "ssh", "10.0.0.1"
	refreshed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return &clientdata.Client{
		ID:                     id,
		ClientAuthID:           "auth-" + id,
		Name:                   "name of " + id,
		OS:                     "Linux 6.1",
		OSArch:                 "amd64",
		OSFamily:               "debian",
		OSKernel:               "linux",
		OSFullName:             "Debian 12",
		OSVersion:              "12",
		OSVirtualizationSystem: "KVM",
		OSVirtualizationRole:   "guest",
		CPUFamily:              "6",
		CPUModel:               "79",
		CPUModelName:           "Intel(R) Xeon(R)",
		CPUVendor:              "GenuineIntel",
		NumCPUs:                4,
		MemoryTotal:            8 << 30,
		Timezone:               "UTC (UTC+00:00)",
		Hostname:               id + ".example.com",
		IPv4:                   []string{"192.0.2.1", "192.0.2.2"},
		IPv6:                   []string{"2001:db8::1"},
		Tags:                   []string{"prod", "eu"},
		Labels:                 map[string]string{"city": "Berlin", "rack": "7"},
		Version:                "1.2.3",
		Address:                "198.51.100.7:51000",
		ConnectedHost:          "server.example.com:8080",
		Tunnels: []*clienttunnel.Tunnel{{
			ID: "1",
			Remote: models.Remote{
				LocalHost: "0.0.0.0", LocalPort: "2222", RemoteHost: "127.0.0.1", RemotePort: "22",
				Scheme: &scheme, ACL: &acl, IdleTimeoutMinutes: 5, AutoClose: time.Minute,
			},
			CreatedAt: refreshed,
		}},
		DisconnectedAt:    disconnectedAt,
		AllowedUserGroups: []string{"admins", "ops"},
		UpdatesStatus: &models.UpdatesStatus{
			Refreshed: refreshed, UpdatesAvailable: 3, SecurityUpdatesAvailable: 1, RebootPending: true,
			UpdateSummaries: []models.UpdateSummary{{Title: "openssl", IsSecurityUpdate: true}},
		},
		IPAddresses:         &models.IPAddresses{IPv4: "203.0.113.1", UpdatedAt: refreshed},
		ClientConfiguration: &clientconfig.Config{InterpreterAliases: map[string]string{"pwsh": "/usr/bin/pwsh"}}, //nolint:gosec // G101: an interpreter path
		Logger:              testLogger,
	}
}

// AssertSameClient checks that got holds every field of want a ClientStore persists.
func AssertSameClient(t *testing.T, want, got *clientdata.Client) {
	t.Helper()
	require.NotNil(t, got)
	assert.JSONEq(t, string(persisted(t, want)), string(persisted(t, got)))
	if want.DisconnectedAt == nil {
		assert.Nil(t, got.DisconnectedAt)
	} else if assert.NotNil(t, got.DisconnectedAt) {
		assert.True(t, want.DisconnectedAt.Equal(*got.DisconnectedAt), "disconnected at: want %s, got %s", want.DisconnectedAt, got.DisconnectedAt)
	}
}

// persisted returns the JSON of the client's persisted fields, with the disconnect time left out (compared apart,
// as stores may return it in another time zone).
func persisted(t *testing.T, c *clientdata.Client) []byte {
	t.Helper()
	cp := &clientdata.Client{
		ID: c.ID, ClientAuthID: c.ClientAuthID, Name: c.Name, OS: c.OS, OSArch: c.OSArch, OSFamily: c.OSFamily,
		OSKernel: c.OSKernel, OSFullName: c.OSFullName, OSVersion: c.OSVersion,
		OSVirtualizationSystem: c.OSVirtualizationSystem, OSVirtualizationRole: c.OSVirtualizationRole,
		CPUFamily: c.CPUFamily, CPUModel: c.CPUModel, CPUModelName: c.CPUModelName, CPUVendor: c.CPUVendor,
		NumCPUs: c.NumCPUs, MemoryTotal: c.MemoryTotal, Timezone: c.Timezone, Hostname: c.Hostname,
		IPv4: c.IPv4, IPv6: c.IPv6, Tags: c.Tags, Labels: c.Labels, Version: c.Version, Address: c.Address,
		ConnectedHost: c.ConnectedHost, Tunnels: c.Tunnels, AllowedUserGroups: c.AllowedUserGroups,
		UpdatesStatus: c.UpdatesStatus, IPAddresses: c.IPAddresses, ClientConfiguration: c.ClientConfiguration,
	}
	b, err := json.Marshal(cp)
	require.NoError(t, err)
	return b
}
