package operator

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nodesFake is a Service with fixed nodes (the rest as fakeService).
type nodesFake struct {
	fakeService
	nodes      []Node
	unassigned int64
	detail     *NodeDetail
	devices    []NodeDevice
	total      int
	from, to   time.Time
	limit, off int
}

func (f *nodesFake) Nodes(_ context.Context, from, to time.Time) ([]Node, int64, error) {
	f.calls = append(f.calls, "nodes")
	f.from, f.to = from, to
	return f.nodes, f.unassigned, f.err
}

func (f *nodesFake) Node(_ context.Context, id string, from, to time.Time) (*NodeDetail, error) {
	f.calls = append(f.calls, "node "+id)
	f.from, f.to = from, to
	if f.detail == nil {
		return nil, ErrNodeNotFound
	}
	return f.detail, f.err
}

func (f *nodesFake) NodeDevices(_ context.Context, id string, limit, offset int) ([]NodeDevice, int, error) {
	f.calls = append(f.calls, "node devices "+id)
	f.limit, f.off = limit, offset
	if f.detail == nil {
		return nil, 0, ErrNodeNotFound
	}
	return f.devices, f.total, f.err
}

func atNoon(t *testing.T) {
	t.Helper()
	old := now
	now = func() time.Time { return time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { now = old })
}

func TestNodeStatus(t *testing.T) {
	assert.Equal(t, NodeLive, NodeStatus(0))
	assert.Equal(t, NodeLive, NodeStatus(60*time.Second), "3 heartbeat intervals")
	assert.Equal(t, NodeStale, NodeStatus(61*time.Second))
	assert.Equal(t, NodeStale, NodeStatus(24*time.Hour))
	assert.Equal(t, NodeGone, NodeStatus(24*time.Hour+time.Second))
}

func TestNodesList(t *testing.T) {
	atNoon(t)
	f := &nodesFake{unassigned: 2, nodes: []Node{
		{NodeID: "z-gone", Status: NodeGone},
		{NodeID: "b", Status: NodeLive, Roles: []string{"all"}, OnlineDevices: 3, TransferPeriod: Transfer{In: 10, Out: 20}},
		{NodeID: "a", Status: NodeStale, OnlineDevices: 1, TransferPeriod: Transfer{In: 1, Out: 2}},
	}}
	r := newRouter(f, operatorUser)
	code, out := do(t, r, http.MethodGet, "/operator/nodes?days=7", "")
	require.Equal(t, http.StatusOK, code, out["_raw"])
	d := out["data"].(map[string]any)
	var ids []string
	for _, n := range d["nodes"].([]any) {
		ids = append(ids, n.(map[string]any)["node_id"].(string))
	}
	assert.Equal(t, []string{"b", "a", "z-gone"}, ids, "live, stale, then gone")
	assert.Equal(t, []any{}, d["nodes"].([]any)[1].(map[string]any)["roles"], "unknown roles: an empty list")
	assert.EqualValues(t, 2, d["unassigned_online"])
	assert.EqualValues(t, 7, d["days"])
	assert.Equal(t, map[string]any{"online_devices": 6.0, "transfer_period": map[string]any{"in": 11.0, "out": 22.0}}, d["totals"])
	assert.Equal(t, time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC), f.from, "7 days, today included")
	assert.Equal(t, time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC), f.to)

	code, _ = do(t, r, http.MethodGet, "/operator/nodes", "")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC), f.from, "30 days by default")
	for _, bad := range []string{"0", "91", "x"} {
		code, out = do(t, r, http.MethodGet, "/operator/nodes?days="+bad, "")
		assert.Equal(t, http.StatusBadRequest, code, "%s: %s", bad, out["_raw"])
	}

	f.nodes = nil
	code, out = do(t, r, http.MethodGet, "/operator/nodes", "")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, []any{}, out["data"].(map[string]any)["nodes"], "an empty list, not null")
}

func TestNodeDetailFillsDays(t *testing.T) {
	atNoon(t)
	f := &nodesFake{detail: &NodeDetail{Node: Node{NodeID: "n1", Status: NodeLive},
		Daily: []NodeDay{{Day: "2026-11-25", In: 5, Out: 6, Connects: 1}}}}
	r := newRouter(f, operatorUser)
	code, out := do(t, r, http.MethodGet, "/operator/nodes/n1?days=3", "")
	require.Equal(t, http.StatusOK, code, out["_raw"])
	d := out["data"].(map[string]any)
	assert.Equal(t, []any{
		map[string]any{"day": "2026-11-24", "in": 0.0, "out": 0.0, "connects": 0.0, "disconnects": 0.0},
		map[string]any{"day": "2026-11-25", "in": 5.0, "out": 6.0, "connects": 1.0, "disconnects": 0.0},
		map[string]any{"day": "2026-11-26", "in": 0.0, "out": 0.0, "connects": 0.0, "disconnects": 0.0},
	}, d["daily"])
	assert.Equal(t, []any{}, d["organizations"])
	assert.Equal(t, []any{}, d["events"])
	assert.Equal(t, []any{}, d["node"].(map[string]any)["roles"])

	f.detail = nil
	code, out = do(t, r, http.MethodGet, "/operator/nodes/nope", "")
	assert.Equal(t, http.StatusNotFound, code, out["_raw"])
	code, out = do(t, r, http.MethodGet, "/operator/nodes/nope/devices", "")
	assert.Equal(t, http.StatusNotFound, code, out["_raw"])
}

func TestNodeDevicesPaging(t *testing.T) {
	f := &nodesFake{detail: &NodeDetail{}, total: 7, devices: []NodeDevice{{Organization: "acme", OrganizationName: "Acme", Device: Device{ID: "d1", Name: "one", Online: true}}}}
	r := newRouter(f, operatorUser)
	code, out := do(t, r, http.MethodGet, "/operator/nodes/n1/devices", "")
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, 50, f.limit)
	assert.Equal(t, 0, f.off)
	assert.Equal(t, map[string]any{"count": 7.0}, out["meta"])
	dev := out["data"].([]any)[0].(map[string]any)
	assert.Equal(t, "acme", dev["organization"])
	assert.Equal(t, "Acme", dev["organization_name"])
	assert.Equal(t, "d1", dev["id"])
	for _, k := range []string{"address", "remote_address", "tags", "labels", "attributes"} {
		assert.NotContains(t, dev, k)
	}

	code, _ = do(t, r, http.MethodGet, "/operator/nodes/n1/devices?page[limit]=5&page[offset]=10", "")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, 5, f.limit)
	assert.Equal(t, 10, f.off)
	for _, bad := range []string{"page[limit]=0", "page[limit]=501", "page[offset]=-1", "page[limit]=x"} {
		code, out = do(t, r, http.MethodGet, "/operator/nodes/n1/devices?"+bad, "")
		assert.Equal(t, http.StatusBadRequest, code, "%s: %s", bad, out["_raw"])
	}
}

func TestNodeRoutesOnlyForOperators(t *testing.T) {
	for _, who := range []testUser{orgAdmin, systemViewer} {
		f := &nodesFake{detail: &NodeDetail{}}
		r := newRouter(f, who)
		for _, p := range []string{"/operator/nodes", "/operator/nodes/n1", "/operator/nodes/n1/devices"} {
			code, out := do(t, r, http.MethodGet, p, "")
			assert.Contains(t, []int{http.StatusNotFound, http.StatusForbidden}, code, fmt.Sprintf("%s as %s: %s", p, who.user.Username, out["_raw"]))
		}
		assert.Empty(t, f.calls)
	}
}

func (f *fakeService) Nodes(context.Context, time.Time, time.Time) ([]Node, int64, error) {
	f.calls = append(f.calls, "nodes")
	return nil, 0, f.err
}

func (f *fakeService) Node(_ context.Context, id string, _, _ time.Time) (*NodeDetail, error) {
	f.calls = append(f.calls, "node "+id)
	return nil, ErrNodeNotFound
}

func (f *fakeService) NodeDevices(_ context.Context, id string, _, _ int) ([]NodeDevice, int, error) {
	f.calls = append(f.calls, "node devices "+id)
	return nil, 0, ErrNodeNotFound
}
