package clientsessions

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/peer"
	"github.com/pohcee/procy-server/server/storage/postgres/pgtest"
	"github.com/pohcee/procy-server/share/tenant"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func ctxFor(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

func newStore(t *testing.T) (*Store, *pgtest.DB) {
	t.Helper()
	d := pgtest.New(t, "a", "b")
	s := New(d.App)
	ctx := context.Background()
	require.NoError(t, s.Heartbeat(ctx, peer.NodeInfo{NodeID: "n1", Region: "r1", PeerAddress: "10.0.0.1:7443"}))
	require.NoError(t, s.Heartbeat(ctx, peer.NodeInfo{NodeID: "n2", PeerAddress: "10.0.0.2:7443"}))
	return s, d
}

func sess(tid tenant.ID, client, id string) peer.Session {
	return peer.Session{Tenant: tid, ClientID: client, SessionID: id}
}

func TestLocateRegisteredSession(t *testing.T) {
	s, _ := newStore(t)
	require.NoError(t, s.Register(context.Background(), "n1", sess("a", "c1", "session-0001"), time.Minute))

	loc, err := s.Locate(ctxFor("a"), "c1")
	require.NoError(t, err)
	assert.Equal(t, &peer.Location{NodeID: "n1", PeerAddress: "10.0.0.1:7443"}, loc)

	// another tenant doesn't see it; an unknown client has no location
	loc, err = s.Locate(ctxFor("b"), "c1")
	require.NoError(t, err)
	assert.Nil(t, loc)
	loc, err = s.Locate(ctxFor("a"), "c2")
	require.NoError(t, err)
	assert.Nil(t, loc)

	_, err = s.Locate(context.Background(), "c1")
	assert.ErrorIs(t, err, tenant.ErrMissing)
}

func TestNewestSessionWins(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	older, newer := sess("a", "c1", "session-older"), sess("a", "c1", "session-newer")
	require.NoError(t, s.Register(ctx, "n1", older, time.Minute))
	require.NoError(t, s.Register(ctx, "n2", newer, time.Minute))

	loc, err := s.Locate(ctxFor("a"), "c1")
	require.NoError(t, err)
	assert.Equal(t, "n2", loc.NodeID)

	// the older holder learns it lost the session when renewing; the newer keeps it
	lost, err := s.Renew(ctx, "n1", []peer.Session{older}, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, []string{"session-older"}, lost)
	lost, err = s.Renew(ctx, "n2", []peer.Session{newer}, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, lost)

	// the older connection ending leaves the newer session in place
	require.NoError(t, s.End(ctx, "session-older"))
	loc, err = s.Locate(ctxFor("a"), "c1")
	require.NoError(t, err)
	assert.Equal(t, "n2", loc.NodeID)

	require.NoError(t, s.End(ctx, "session-newer"))
	loc, err = s.Locate(ctxFor("a"), "c1")
	require.NoError(t, err)
	assert.Nil(t, loc)
}

func TestLeaseExpiry(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	held := sess("a", "c1", "session-0001")
	require.NoError(t, s.Register(ctx, "n1", held, time.Second))
	loc, err := s.Locate(ctxFor("a"), "c1")
	require.NoError(t, err)
	require.NotNil(t, loc)

	require.Eventually(t, func() bool {
		got, err := s.Locate(ctxFor("a"), "c1")
		return err == nil && got == nil
	}, 5*time.Second, 100*time.Millisecond, "an expired session is not located")

	// renewing an expired, not replaced session takes it back
	lost, err := s.Renew(ctx, "n1", []peer.Session{held}, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, lost)
	loc, err = s.Locate(ctxFor("a"), "c1")
	require.NoError(t, err)
	assert.Equal(t, "n1", loc.NodeID)
}

func TestRenewRecordsMissingSessionUnlessReplaced(t *testing.T) {
	s, d := newStore(t)
	ctx := context.Background()
	held := sess("a", "c1", "session-0001")
	// the row is gone (removed after expiry): renewing records it again
	lost, err := s.Renew(ctx, "n1", []peer.Session{held}, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, lost)
	loc, err := s.Locate(ctxFor("a"), "c1")
	require.NoError(t, err)
	assert.Equal(t, "n1", loc.NodeID)

	// long expired rows are removed by renewing
	_, err = d.Owner.Exec("UPDATE client_sessions SET lease_until = now() - interval '2 hours'")
	require.NoError(t, err)
	_, err = s.Renew(ctx, "n2", nil, time.Minute)
	require.NoError(t, err)
	var n int
	require.NoError(t, d.Owner.Get(&n, "SELECT count(*) FROM client_sessions"))
	assert.Zero(t, n)
}

func TestRegistryRefusesBadInput(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"bad node id":     func() error { return s.Register(ctx, "bad node", sess("a", "c1", "session-0001"), time.Minute) },
		"empty client":    func() error { return s.Register(ctx, "n1", sess("a", "", "session-0001"), time.Minute) },
		"short session":   func() error { return s.Register(ctx, "n1", sess("a", "c1", "x"), time.Minute) },
		"unknown tenant":  func() error { return s.Register(ctx, "n1", sess("zz", "c1", "session-0001"), time.Minute) },
		"reserved tenant": func() error { return s.Register(ctx, "n1", sess("_unresolved", "c1", "session-0001"), time.Minute) },
		"long lease":      func() error { return s.Register(ctx, "n1", sess("a", "c1", "session-0001"), 2*time.Hour) },
	} {
		assert.Error(t, call(), name)
	}
}

func TestRenewReportsUnknownTenantLost(t *testing.T) {
	s, _ := newStore(t)
	lost, err := s.Renew(context.Background(), "n1", []peer.Session{sess("zz", "c1", "session-0001")}, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, []string{"session-0001"}, lost)
}

func TestApplicationRoleCannotWriteRegistryTables(t *testing.T) {
	_, d := newStore(t)
	for _, q := range []string{
		"INSERT INTO nodes (node_id, peer_address) VALUES ('x', 'y')",
		"UPDATE nodes SET peer_address = 'evil:1'",
		"DELETE FROM client_sessions",
		"INSERT INTO client_sessions (tenant_id, client_id, node_id, session_id, lease_until) VALUES ('a', 'c', 'n1', 'session-0001', now())",
	} {
		_, err := d.App.Exec(q)
		assert.ErrorContains(t, err, "permission denied", q)
	}
	// a role without the grant can't call the functions
	nobody := fmt.Sprintf("rport_nobody_%d", time.Now().UnixNano())
	_, err := d.Owner.Exec("CREATE ROLE " + nobody + " NOLOGIN")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = d.Owner.Exec("DROP ROLE " + nobody) })
	tx, err := d.Owner.Beginx()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec("SET LOCAL ROLE " + nobody)
	require.NoError(t, err)
	_, err = tx.Exec("SELECT rport_end_session('session-0001')")
	assert.ErrorContains(t, err, "permission denied")
}

// A session that can't be held any more (its tenant was deleted) is reported lost; the others are renewed.
func TestRenewSkipsSessionOfDeletedTenant(t *testing.T) {
	s, d := newStore(t)
	ctx := context.Background()
	good, gone := sess("a", "c1", "session-0001"), sess("b", "c2", "session-0002")
	require.NoError(t, s.Register(ctx, "n1", good, time.Minute))
	require.NoError(t, s.Register(ctx, "n1", gone, time.Minute))
	_, err := d.Owner.Exec("DELETE FROM tenants WHERE id = 'b'")
	require.NoError(t, err)

	lost, err := s.Renew(ctx, "n1", []peer.Session{good, gone}, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, []string{"session-0002"}, lost)
	loc, err := s.Locate(ctxFor("a"), "c1")
	require.NoError(t, err)
	assert.NotNil(t, loc)
}

// Renewing stores a held client connected again when the sweep marked it disconnected while its lease had lapsed.
func TestRenewRestoresConnectedState(t *testing.T) {
	s, d := newStore(t)
	ctx := context.Background()
	_, err := d.Owner.Exec(`INSERT INTO clients (tenant_id, id, client_auth_id, disconnected_at, details)
		VALUES ('a', 'c1', 'auth-c1', now(), '{}')`)
	require.NoError(t, err)
	held := sess("a", "c1", "session-0001")
	_, err = s.Renew(ctx, "n1", []peer.Session{held}, time.Minute)
	require.NoError(t, err)
	var connected bool
	require.NoError(t, d.Owner.Get(&connected, "SELECT disconnected_at IS NULL FROM clients WHERE id = 'c1'"))
	assert.True(t, connected)
}

type nodeReport struct {
	Version   *string    `db:"version"`
	Roles     *string    `db:"roles"`
	StartedAt *time.Time `db:"started_at"`
}

func reportOf(t *testing.T, d *pgtest.DB, node string) nodeReport {
	t.Helper()
	var r nodeReport
	require.NoError(t, d.Owner.Get(&r, "SELECT version, array_to_string(roles, ',') AS roles, started_at FROM nodes WHERE node_id = $1", node))
	return r
}

// The heartbeat of this release reports the version, roles and process start, also with the device role; the
// 3-argument heartbeat of the previous release clears them.
func TestHeartbeatReportsVersionRolesAndStart(t *testing.T) {
	d := pgtest.New(t)
	ctx := context.Background()
	started := time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC)
	s := New(d.Device)
	require.NoError(t, s.Heartbeat(ctx, peer.NodeInfo{NodeID: "n1", Region: "r1", PeerAddress: "10.0.0.1:7443",
		Version: "0.17.0", Roles: []string{"device"}, StartedAt: started}))
	r := reportOf(t, d, "n1")
	require.NotNil(t, r.Version)
	assert.Equal(t, "0.17.0", *r.Version)
	require.NotNil(t, r.Roles)
	assert.Equal(t, "device", *r.Roles)
	require.NotNil(t, r.StartedAt)
	assert.True(t, started.Equal(*r.StartedAt))

	// a node of the previous release (rolling upgrade, rollback)
	_, err := d.Device.Exec("SELECT rport_node_heartbeat('n1', 'r1', '10.0.0.1:7443')")
	require.NoError(t, err)
	assert.Equal(t, nodeReport{}, reportOf(t, d, "n1"))

	// nothing reported: NULLs, the heartbeat still counts
	require.NoError(t, New(d.App).Heartbeat(ctx, peer.NodeInfo{NodeID: "n2"}))
	r = reportOf(t, d, "n2")
	assert.Nil(t, r.Version)
	assert.Nil(t, r.StartedAt)

	// a long build version is cut, not refused (the heartbeat must go on)
	require.NoError(t, s.Heartbeat(ctx, peer.NodeInfo{NodeID: "n3", Version: strings.Repeat("v", 70)}))
	r = reportOf(t, d, "n3")
	require.NotNil(t, r.Version)
	assert.Len(t, *r.Version, 64)
	_, err = d.Device.Exec("SELECT rport_node_heartbeat('n1', '', '', $1, '{}', NULL)", strings.Repeat("v", 65))
	assert.Error(t, err, "the database refuses more")

	for _, bad := range []peer.NodeInfo{
		{NodeID: "n1", Roles: []string{"Bad Role"}},
		{NodeID: "n1", Roles: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}},
		{NodeID: "bad node"},
	} {
		assert.Error(t, s.Heartbeat(ctx, bad), "%+v", bad)
	}
}

// The 6-argument heartbeat is not SECURITY DEFINER (it calls the 3-argument one), so the device role of the
// previous release, whose check refuses any SECURITY DEFINER function it doesn't know, still passes it.
func TestHeartbeatAddsNoSecurityDefinerFunction(t *testing.T) {
	d := pgtest.New(t)
	var definer, may bool
	require.NoError(t, d.Owner.Get(&definer, "SELECT prosecdef FROM pg_proc WHERE oid = 'rport_node_heartbeat(text, text, text, text, text[], timestamptz)'::regprocedure"))
	assert.False(t, definer)
	require.NoError(t, d.Owner.Get(&may, "SELECT has_function_privilege($1, 'rport_node_heartbeat(text, text, text, text, text[], timestamptz)', 'EXECUTE')", pgtest.DeviceRole))
	assert.True(t, may)
	require.NoError(t, d.Owner.Get(&may, "SELECT has_function_privilege('public', 'rport_node_heartbeat(text, text, text, text, text[], timestamptz)', 'EXECUTE')"))
	assert.False(t, may, "not for PUBLIC")
}
