package pgserver

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/billing"
	"github.com/openrport/openrport/server/clientsauth"
	pgclientauth "github.com/openrport/openrport/server/storage/postgres/clientauth"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	"github.com/openrport/openrport/share/tenant"
)

const nodePlans = `
free_plan = "free"
[[plan]]
  id = "free"
  name = "Free"
  max_clients = 10
  transfer_base = "1MiB"
  overage = "cap"
[[plan]]
  id = "team"
  name = "Team"
  base_price = 49.0
  transfer_base = "250GiB"
  overage = "bill"
  overage_price_per_gib = 0.2
`

func billingOn(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plans.toml")
	require.NoError(t, os.WriteFile(path, []byte(nodePlans), 0o600))
	return meteringOn + fmt.Sprintf("[billing]\n  enabled = true\n  plans_file = %q\n", path)
}

func shortBillingIntervals(t *testing.T) {
	t.Helper()
	pr, ei := billing.PolicyRefresh, billing.EnforceInterval
	billing.PolicyRefresh, billing.EnforceInterval = 200*time.Millisecond, time.Second
	t.Cleanup(func() { billing.PolicyRefresh, billing.EnforceInterval = pr, ei })
}

// TestBillingEndToEnd runs a node with [billing] on and a real client of organization a: the administrators read
// their plan, operators set it, and at the hard cap new tunnels and uploads are refused while the device stays
// connected and commands keep working.
func TestBillingEndToEnd(t *testing.T) {
	shortBillingIntervals(t)
	d := pgtest.New(t)
	seedTenants(t, d)
	ctxA := tenant.WithID(context.Background(), "a")
	_, err := pgclientauth.New(d.App).Add(ctxA, &clientsauth.ClientAuth{ID: peerAuthID, Password: peerAuthPass})
	require.NoError(t, err)
	require.NoError(t, AddUser(ctxA, d.App, "a", "carl", testPassword, []string{"Viewers"}))

	listen := freeAddr(t)
	h := startNodeOn(t, d, nodeSettings{role: "all", listen: listen, extra: billingOn(t)})
	alice, carl, op := h.login(t, "alice"), h.login(t, "carl"), h.login(t, "op")
	assert.Equal(t, true, capabilities(t, h, alice)["billing"])

	code, out := h.do(t, http.MethodGet, "/billing", carl, nil)
	assert.Equal(t, http.StatusForbidden, code, "administrators only: %v", out["_raw"])
	require.Eventually(t, func() bool {
		code, out = h.do(t, http.MethodGet, "/billing", alice, nil)
		return code == http.StatusOK && data(t, out)["transfer_state_at"] != nil
	}, 20*time.Second, 100*time.Millisecond, "the upkeep wrote the policy")
	acc := data(t, out)
	assert.Equal(t, "free", acc["plan"])
	assert.Equal(t, "paying", acc["status"])
	assert.Equal(t, "Free", acc["plan_details"].(map[string]any)["name"])

	assert.Equal(t, true, acc["plan_unassigned"], "no plan chosen yet: no limits")
	// operators change the plan and status
	code, out = h.do(t, http.MethodPut, "/operator/organizations/a/billing/plan", op, map[string]any{"plan": "free"})
	require.Equal(t, http.StatusOK, code, out["_raw"])
	code, out = h.do(t, http.MethodPut, "/operator/organizations/b/billing/plan", op, map[string]any{"plan": "team"})
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, "team", data(t, out)["plan"])
	code, out = h.do(t, http.MethodPut, "/operator/organizations/b/billing/status", op, map[string]any{"status": "exempt", "reason": "partner"})
	require.Equal(t, http.StatusOK, code, out["_raw"])
	code, out = h.do(t, http.MethodPut, "/operator/organizations/b/billing/plan", op, map[string]any{"plan": "gold"})
	assert.Equal(t, http.StatusBadRequest, code, out["_raw"])
	code, _ = h.do(t, http.MethodPut, "/operator/organizations/nope/billing/plan", op, map[string]any{"plan": "team"})
	assert.Equal(t, http.StatusNotFound, code)
	code, _ = h.do(t, http.MethodGet, "/operator/billing", alice, nil)
	assert.Equal(t, http.StatusNotFound, code, "not for organizations")
	code, out = h.do(t, http.MethodGet, "/operator/billing", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	code, out = h.do(t, http.MethodGet, "/operator/billing/periods?state=held", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	// a free plan has no spending limit
	code, out = h.do(t, http.MethodPut, "/billing/spending-limit", alice, map[string]any{"spending_limit": 10})
	assert.Equal(t, http.StatusBadRequest, code, out["_raw"])

	connectClient(t, listen)
	require.Eventually(t, func() bool { return clientState(t, h, alice) == "connected" }, 20*time.Second, 100*time.Millisecond)
	target := sizedServer(t, 1, 1)
	code, out = h.do(t, http.MethodPut, "/clients/"+peerClientID+"/tunnels?remote="+target+"&scheme=other", alice, nil)
	require.Equal(t, http.StatusOK, code, "below the cap: %v", out["_raw"])

	// a's transfer passes its 1 MiB allowance: capped
	_, err = d.Owner.Exec("INSERT INTO usage_hourly (tenant_id, hour, metric, value) VALUES ('a', date_trunc('hour', now()), 'tunnel_bytes_to_user', 2097152)")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		code, out = h.do(t, http.MethodPut, "/clients/"+peerClientID+"/tunnels?remote="+sizedServer(t, 1, 1)+"&scheme=other", alice, nil)
		return code == http.StatusForbidden
	}, 20*time.Second, 200*time.Millisecond, "new tunnels are refused at the hard cap")
	assert.Contains(t, out["_raw"], "ERR_CODE_TRANSFER_LIMIT")
	assert.Contains(t, out["_raw"], `plan \"Free\"`)
	code, out = h.do(t, http.MethodGet, "/billing", alice, nil)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "capped", data(t, out)["transfer_state"])
	assert.Equal(t, true, data(t, out)["soft_cap_reached"])

	req, err := http.NewRequest(http.MethodPost, h.base+"/files", bytes.NewReader(nil))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+alice)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "uploads are refused")

	// the device stays connected and commands work
	assert.Equal(t, "connected", clientState(t, h, alice))
	code, out = h.do(t, http.MethodPost, "/clients/"+peerClientID+"/commands", alice, map[string]any{"command": "echo still", "timeout_sec": 10})
	assert.Equal(t, http.StatusOK, code, out["_raw"])

	// an operator lifts the cap
	code, out = h.do(t, http.MethodPut, "/operator/organizations/a/billing/override", op,
		map[string]any{"cap_lifted_until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	require.Equal(t, http.StatusOK, code, out["_raw"])
	require.Eventually(t, func() bool {
		code, _ = h.do(t, http.MethodPut, "/clients/"+peerClientID+"/tunnels?remote="+sizedServer(t, 1, 1)+"&scheme=other", alice, nil)
		return code == http.StatusOK
	}, 20*time.Second, 200*time.Millisecond)
}
