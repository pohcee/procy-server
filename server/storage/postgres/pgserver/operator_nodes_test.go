package pgserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/clientsauth"
	"github.com/pohcee/procy-server/server/metering"
	"github.com/pohcee/procy-server/server/operator"
	"github.com/pohcee/procy-server/server/peer"
	pgclientauth "github.com/pohcee/procy-server/server/storage/postgres/clientauth"
	"github.com/pohcee/procy-server/server/storage/postgres/clientsessions"
	"github.com/pohcee/procy-server/server/storage/postgres/pgtest"
	"github.com/pohcee/procy-server/server/storage/postgres/usage"
	"github.com/pohcee/procy-server/share/tenant"
)

func utcToday() time.Time {
	n := time.Now().UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

func setupNodes(t *testing.T) *Operator {
	t.Helper()
	o, d := newOperator(t)
	ctx := context.Background()
	for _, id := range []string{"acme", "beta"} {
		_, err := o.Create(ctx, ops, operator.NewOrganization{ID: id, Name: strings.ToUpper(id[:1]) + id[1:]})
		require.NoError(t, err)
	}
	exec := func(q string, args ...any) { t.Helper(); mustExec(t, d.Owner, q, args...) }
	started := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	reg := clientsessions.New(d.App)
	require.NoError(t, reg.Heartbeat(ctx, peer.NodeInfo{NodeID: "n1", Region: "us-central1", PeerAddress: "10.0.0.1:7443",
		Version: "0.17.0", Roles: []string{"device"}, StartedAt: started}))
	exec(`INSERT INTO nodes (node_id, region, heartbeat_at) VALUES ('old', 'eu', now() - interval '2 days'), ('mid', '', now() - interval '1 hour')`)
	exec(`INSERT INTO client_state (tenant_id, client_id, client_auth_id, disconnected_at, details, reported_at) VALUES
		('acme', 'web', 'auth', NULL, '{"name":"Web","version":"0.16.0","os_full_name":"Ubuntu 24.04","os":"linux","os_family":"debian","remote_address":"198.51.100.7","tags":["secret"],"labels":{"k":"v"}}', now()),
		('acme', 'db', 'auth', NULL, '{"name":"alpha","version":"0.16.0","os":"linux"}', now()),
		('beta', 'b1', 'auth', NULL, '{"name":"Beta one","os":"windows"}', now())`)
	exec(`INSERT INTO client_sessions (tenant_id, client_id, node_id, session_id, connected_at, lease_until) VALUES
		('acme', 'web', 'n1', 'sess0001', now() - interval '10 minutes', now() + interval '1 minute'),
		('acme', 'db', 'n1', 'sess0002', now() - interval '5 minutes', now() + interval '1 minute'),
		('beta', 'b1', 'n1', 'sess0003', now() - interval '1 minute', now() + interval '1 minute'),
		('acme', 'lapsed', 'n1', 'sess0004', now() - interval '1 day', now() - interval '1 minute'),
		('acme', 'elsewhere', 'ghost', 'sess0005', now(), now() + interval '1 minute')`)
	// minutes ago, but today (UTC) also just after midnight, in the same order
	ago := func(m int) string {
		return fmt.Sprintf("greatest(now() - interval '%d minutes', date_trunc('day', now(), 'UTC') + interval '%d seconds')", m, 60-m)
	}
	exec(`INSERT INTO client_connection_events (tenant_id, client_id, client_auth_id, event, at, node_id) VALUES
		('acme', 'web', 'auth', 'connected', ` + ago(10) + `, 'n1'),
		('acme', 'db', 'auth', 'connected', ` + ago(5) + `, 'n1'),
		('beta', 'b1', 'auth', 'connected', ` + ago(1) + `, 'n1'),
		('acme', 'db', 'auth', 'disconnected', ` + ago(6) + `, 'n1'),
		('acme', 'web', 'auth', 'connected', now() - interval '2 days', 'n1'),
		('acme', 'web', 'auth', 'connected', now() - interval '1 minute', 'mid')`)

	// transfer through ingest_usage: n1 today and two days ago, a record without a node, one with an invalid node
	h := time.Now().UTC().Truncate(time.Hour)
	twoDaysAgo := h.Add(-48 * time.Hour)
	w := usage.Writer{DB: d.App}
	require.NoError(t, w.WriteUsage(ctx, []metering.Record{
		{Tenant: "acme", Node: "n1", Key: "k1", Deltas: []metering.Delta{
			{Hour: h, Metric: metering.TunnelBytesToDevice, Value: 100},
			{Hour: h, Metric: metering.UploadBytesToDevices, Value: 10},
			{Hour: h, Metric: metering.TunnelBytesToUser, Value: 1000},
			{Hour: h, Metric: metering.JobsCommand, Value: 3},
			{Hour: twoDaysAgo, Metric: metering.TunnelBytesToUser, Value: 7}}},
		{Tenant: "beta", Node: "n1", Key: "k2", Deltas: []metering.Delta{{Hour: h, Metric: metering.APIBytesOut, Value: 5}}},
		{Tenant: "acme", Key: "k3", Deltas: []metering.Delta{{Hour: h, Metric: metering.TunnelBytesToUser, Value: 999}}},
		{Tenant: "acme", Node: "bad node!", Key: "k4", Deltas: []metering.Delta{{Hour: h, Metric: metering.TunnelBytesToUser, Value: 999}}},
	}))
	return o
}

func TestOperatorNodesList(t *testing.T) {
	o := setupNodes(t)
	ctx := context.Background()
	to := utcToday().AddDate(0, 0, 1)
	nodes, unassigned, err := o.Nodes(ctx, to.AddDate(0, 0, -30), to)
	require.NoError(t, err)
	assert.EqualValues(t, 1, unassigned, "the session on ghost")
	byID := map[string]operator.Node{}
	for _, n := range nodes {
		byID[n.NodeID] = n
	}
	require.Len(t, byID, 3)

	n1 := byID["n1"]
	assert.Equal(t, operator.NodeLive, n1.Status)
	assert.Equal(t, "us-central1", n1.Region)
	assert.Equal(t, "10.0.0.1:7443", n1.PeerAddress)
	assert.Equal(t, "0.17.0", n1.Version)
	assert.Equal(t, []string{"device"}, n1.Roles)
	require.NotNil(t, n1.StartedAt)
	assert.WithinDuration(t, time.Now().Add(-time.Hour), *n1.StartedAt, 2*time.Second)
	assert.WithinDuration(t, time.Now(), n1.HeartbeatAt, time.Minute)
	assert.EqualValues(t, 3, n1.OnlineDevices, "the lapsed session doesn't count")
	assert.EqualValues(t, 2, n1.Organizations)
	assert.EqualValues(t, 3, n1.Connects24h)
	assert.EqualValues(t, 1, n1.Disconnects24h)
	assert.Equal(t, operator.Transfer{In: 110, Out: 1005}, n1.TransferToday)
	assert.Equal(t, operator.Transfer{In: 110, Out: 1012}, n1.TransferPeriod, "records without a (valid) node are not attributed")
	require.NotNil(t, n1.MeteredSince)
	assert.Equal(t, utcToday().AddDate(0, 0, -2).Format(time.DateOnly), *n1.MeteredSince)

	old := byID["old"]
	assert.Equal(t, operator.NodeGone, old.Status)
	assert.Equal(t, "", old.Version, "a node of a release that doesn't report it")
	assert.Equal(t, []string{}, old.Roles)
	assert.Nil(t, old.StartedAt)
	assert.Nil(t, old.MeteredSince)
	assert.Zero(t, old.OnlineDevices)
	assert.Equal(t, operator.NodeStale, byID["mid"].Status)
	assert.EqualValues(t, 1, byID["mid"].Connects24h)

	// a period of 1 day leaves out the transfer of two days ago
	nodes, _, err = o.Nodes(ctx, to.AddDate(0, 0, -1), to)
	require.NoError(t, err)
	for _, n := range nodes {
		if n.NodeID == "n1" {
			assert.Equal(t, operator.Transfer{In: 110, Out: 1005}, n.TransferPeriod)
		}
	}
}

func TestOperatorNodeDetail(t *testing.T) {
	o := setupNodes(t)
	ctx := context.Background()
	to := utcToday().AddDate(0, 0, 1)
	d, err := o.Node(ctx, "n1", to.AddDate(0, 0, -7), to)
	require.NoError(t, err)
	assert.Equal(t, "n1", d.Node.NodeID)
	assert.EqualValues(t, 3, d.Node.OnlineDevices)
	byDay := map[string]operator.NodeDay{}
	for _, x := range d.Daily {
		byDay[x.Day] = x
	}
	today, twoAgo := utcToday().Format(time.DateOnly), utcToday().AddDate(0, 0, -2).Format(time.DateOnly)
	assert.Equal(t, operator.NodeDay{Day: today, In: 110, Out: 1005, Connects: 3, Disconnects: 1}, byDay[today])
	assert.Equal(t, operator.NodeDay{Day: twoAgo, Out: 7, Connects: 1}, byDay[twoAgo])
	assert.Equal(t, []operator.NodeOrganization{
		{Organization: "acme", OrganizationName: "Acme", OnlineDevices: 2},
		{Organization: "beta", OrganizationName: "Beta", OnlineDevices: 1},
	}, d.Organizations)
	require.Len(t, d.Events, 5)
	assert.Equal(t, operator.NodeEvent{At: d.Events[0].At, Organization: "beta", ClientID: "b1", DeviceName: "Beta one", Event: "connected"}, d.Events[0])
	assert.Equal(t, "disconnected", d.Events[2].Event, "newest first")
	assert.Equal(t, "alpha", d.Events[2].DeviceName)

	_, err = o.Node(ctx, "nope", to.AddDate(0, 0, -7), to)
	assert.ErrorIs(t, err, operator.ErrNodeNotFound)
	_, _, err = o.NodeDevices(ctx, "ghost", 50, 0)
	assert.ErrorIs(t, err, operator.ErrNodeNotFound, "sessions on a node not in the registry")
}

func TestOperatorNodeDevicesPaging(t *testing.T) {
	o := setupNodes(t)
	ctx := context.Background()
	ds, total, err := o.NodeDevices(ctx, "n1", 50, 0)
	require.NoError(t, err)
	assert.Equal(t, 3, total)
	require.Len(t, ds, 3)
	var got []string
	for _, x := range ds {
		got = append(got, x.Organization+"/"+x.Name)
	}
	assert.Equal(t, []string{"acme/alpha", "acme/Web", "beta/Beta one"}, got, "by organization, then name")
	web := ds[1]
	assert.Equal(t, "Acme", web.OrganizationName)
	assert.Equal(t, "web", web.ID)
	assert.True(t, web.Online)
	require.NotNil(t, web.NodeID)
	assert.Equal(t, "n1", *web.NodeID)
	assert.Equal(t, "Ubuntu 24.04", web.OS)
	assert.Equal(t, "debian", web.OSFamily)
	assert.Equal(t, "0.16.0", web.Version)
	require.NotNil(t, web.ConnectedAt)
	require.NotNil(t, web.FirstSeenAt)
	b, err := json.Marshal(web)
	require.NoError(t, err)
	for _, secret := range []string{"198.51.100.7", "secret", "labels", "remote_address"} {
		assert.NotContains(t, string(b), secret, "no addresses, tags or labels")
	}

	page, total, err := o.NodeDevices(ctx, "n1", 2, 1)
	require.NoError(t, err)
	assert.Equal(t, 3, total)
	require.Len(t, page, 2)
	assert.Equal(t, "web", page[0].ID)
	assert.Equal(t, "b1", page[1].ID)
	page, total, err = o.NodeDevices(ctx, "n1", 2, 10)
	require.NoError(t, err)
	assert.Equal(t, 3, total, "the count past the last page")
	assert.Empty(t, page)
	page, total, err = o.NodeDevices(ctx, "old", 2, 0)
	require.NoError(t, err)
	assert.Zero(t, total)
	assert.Empty(t, page)
}

func TestOperatorNodeReadsOnlyInSystemTenant(t *testing.T) {
	_, d := newOperator(t)
	d.AddTenants(t, "acme")
	for _, q := range []string{
		"SELECT count(*) FROM rport_operator_nodes(current_date, current_date + 1)",
		"SELECT rport_operator_unassigned_sessions()",
		"SELECT count(*) FROM rport_operator_node_days('n1', current_date, current_date + 1)",
		"SELECT count(*) FROM rport_operator_node_organizations('n1')",
		"SELECT count(*) FROM rport_operator_node_events('n1', 50)",
		"SELECT count(*) FROM rport_operator_node_devices('n1', 50, 0)",
	} {
		_, err := d.App.Exec(q)
		assert.ErrorContains(t, err, "platform operators", "outside a transaction: %s", q)
		tx, err := d.App.Beginx()
		require.NoError(t, err)
		_, err = tx.Exec("SELECT set_config('rport.tenant_id', 'acme', true)")
		require.NoError(t, err)
		_, err = tx.Exec(q)
		assert.ErrorContains(t, err, "platform operators", "in an organization's tenant: %s", q)
		require.NoError(t, tx.Rollback())
	}
	var n int
	require.NoError(t, d.App.Get(&n, "SELECT count(*) FROM usage_node_daily"), "the application role reads it (operator code paths)")
	_, err := d.App.Exec("INSERT INTO usage_node_daily (node_id, day, metric, value) VALUES ('n1', current_date, 'api_bytes_out', 1)")
	assert.ErrorContains(t, err, "permission denied", "written only by ingest_usage")
	_, err = d.Device.Exec("SELECT count(*) FROM usage_node_daily")
	assert.ErrorContains(t, err, "permission denied", "the device role can't read it")
}

// TestNodeOperatorNodesAPI runs a node with a client listener: it lists itself through GET /operator/nodes with its
// version and role, next to a node that is gone, and refuses users other than operators.
func TestNodeOperatorNodesAPI(t *testing.T) {
	d := pgtest.New(t)
	seedTenants(t, d)
	mustExec(t, d.Owner, "INSERT INTO nodes (node_id, region, heartbeat_at) VALUES ('old', 'eu', now() - interval '3 days')")
	_, err := pgclientauth.New(d.App).Add(tenant.WithID(context.Background(), "a"), &clientsauth.ClientAuth{ID: peerAuthID, Password: peerAuthPass})
	require.NoError(t, err)
	listen := freeAddr(t)
	h := startNodeOn(t, d, nodeSettings{role: "all", nodeID: "node-all", listen: listen})
	op, alice, viewer := h.login(t, "op"), h.login(t, "alice"), h.login(t, "viewer")
	connectClient(t, listen)
	require.Eventually(t, func() bool { return clientState(t, h, alice) == "connected" }, 20*time.Second, 100*time.Millisecond)

	var out map[string]any
	var code int
	require.Eventually(t, func() bool {
		code, out = h.do(t, http.MethodGet, "/operator/nodes?days=7", op, nil)
		require.Equal(t, http.StatusOK, code, out["_raw"])
		ns := data(t, out)["nodes"].([]any)
		return len(ns) == 2 && ns[0].(map[string]any)["online_devices"] == 1.0
	}, 15*time.Second, 100*time.Millisecond, "the node's heartbeat")
	t.Logf("GET /operator/nodes: %s", out["_raw"])
	list := data(t, out)
	assert.EqualValues(t, 7, list["days"])
	nodes := list["nodes"].([]any)
	first, last := nodes[0].(map[string]any), nodes[1].(map[string]any)
	assert.Equal(t, "node-all", first["node_id"])
	assert.Equal(t, "live", first["status"])
	assert.Equal(t, []any{"all"}, first["roles"])
	assert.NotEmpty(t, first["version"])
	assert.NotNil(t, first["started_at"])
	assert.Equal(t, "old", last["node_id"])
	assert.Equal(t, "gone", last["status"], "still listed, at the bottom")

	code, out = h.do(t, http.MethodGet, "/operator/nodes/node-all?days=3", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	t.Logf("GET /operator/nodes/node-all: %s", out["_raw"])
	assert.Len(t, data(t, out)["daily"], 3, "every day of the period")
	code, out = h.do(t, http.MethodGet, "/operator/nodes/node-all/devices", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	t.Logf("GET /operator/nodes/node-all/devices: %s", out["_raw"])
	assert.Equal(t, map[string]any{"count": 1.0}, out["meta"])
	dev := out["data"].([]any)[0].(map[string]any)
	assert.Equal(t, "a", dev["organization"])
	assert.Equal(t, peerClientID, dev["id"])
	assert.Equal(t, "node-all", dev["node_id"])
	code, out = h.do(t, http.MethodGet, "/operator/nodes/nope", op, nil)
	assert.Equal(t, http.StatusNotFound, code, out["_raw"])

	code, out = h.do(t, http.MethodGet, "/operator/nodes", alice, nil)
	assert.Equal(t, http.StatusNotFound, code, "an organization admin: %s", out["_raw"])
	code, out = h.do(t, http.MethodGet, "/operator/nodes", viewer, nil)
	assert.Equal(t, http.StatusForbidden, code, "a non-admin of the system tenant: %s", out["_raw"])
}
