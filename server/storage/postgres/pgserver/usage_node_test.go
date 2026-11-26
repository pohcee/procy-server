package pgserver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/clientsauth"
	pgclientauth "github.com/pohcee/procy-server/server/storage/postgres/clientauth"
	"github.com/pohcee/procy-server/server/storage/postgres/pgtest"
	"github.com/pohcee/procy-server/share/tenant"
)

const meteringOn = "[metering]\n  enabled = true\n  flush_interval = \"5s\"\n"

// sizedServer reads want bytes from each connection, then writes send bytes and closes it.
func sizedServer(t *testing.T, want, send int) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if _, err := io.CopyN(io.Discard, c, int64(want)); err != nil {
					return
				}
				_, _ = c.Write(bytes.Repeat([]byte{'d'}, send))
			}()
		}
	}()
	return l.Addr().String()
}

func within(t *testing.T, want, got float64, msg string) {
	t.Helper()
	assert.LessOrEqual(t, math.Abs(got-want), want*0.01+16, "%s: want %v, got %v", msg, want, got)
}

// TestUsageMeteredEndToEnd runs a node with [metering] on and a real client of organization a, moves known amounts
// through a tunnel, an upload and a command, and reads them back through the organization's usage API and the
// operator's, per organization.
func TestUsageMeteredEndToEnd(t *testing.T) {
	d := pgtest.New(t)
	seedTenants(t, d)
	ctxA := tenant.WithID(context.Background(), "a")
	_, err := pgclientauth.New(d.App).Add(ctxA, &clientsauth.ClientAuth{ID: peerAuthID, Password: peerAuthPass})
	require.NoError(t, err)
	require.NoError(t, AddUser(ctxA, d.App, "a", "carl", testPassword, []string{"Viewers"})) // not an administrator

	listen := freeAddr(t)
	h := startNodeOn(t, d, nodeSettings{role: "all", listen: listen, extra: meteringOn})
	alice, bob, op := h.login(t, "alice"), h.login(t, "bob"), h.login(t, "op")
	assert.Equal(t, true, capabilities(t, h, alice)["usage"])

	connectClient(t, listen)
	require.Eventually(t, func() bool { return clientState(t, h, alice) == "connected" }, 20*time.Second, 100*time.Millisecond)

	// a tunnel: 100 KiB from the user to the device, 1 MiB back
	const up, down = 100 << 10, 1 << 20
	target := sizedServer(t, up, down)
	code, out := h.do(t, http.MethodPut, "/clients/"+peerClientID+"/tunnels?remote="+target+"&scheme=other", alice, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	tun := data(t, out)
	tc, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(tun["lport"])), 5*time.Second)
	require.NoError(t, err)
	_, err = tc.Write(bytes.Repeat([]byte{'u'}, up))
	require.NoError(t, err)
	require.NoError(t, tc.SetReadDeadline(time.Now().Add(10*time.Second)))
	n, err := io.Copy(io.Discard, tc)
	require.NoError(t, err)
	require.EqualValues(t, down, n)
	tc.Close()

	// an upload of 256 KiB
	const size = 256 << 10
	dest := filepath.Join(t.TempDir(), "metered.bin")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	require.NoError(t, mw.WriteField("client_id", peerClientID))
	require.NoError(t, mw.WriteField("dest", dest))
	fw, err := mw.CreateFormFile("upload", "metered.bin")
	require.NoError(t, err)
	_, err = fw.Write(bytes.Repeat([]byte{'f'}, size))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	req, err := http.NewRequest(http.MethodPost, h.base+"/files", &body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+alice)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.Eventually(t, func() bool {
		st, err := os.Stat(dest)
		return err == nil && st.Size() == size
	}, 20*time.Second, 100*time.Millisecond)

	// a command
	code, out = h.do(t, http.MethodPost, "/clients/"+peerClientID+"/commands", alice, map[string]any{"command": "echo metered", "timeout_sec": 10})
	require.Equal(t, http.StatusOK, code, out["_raw"])

	// the per-tunnel view counts while the tunnel stays open
	code, out = h.do(t, http.MethodGet, "/clients/"+peerClientID+"?fields[clients]=tunnels", alice, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	list, _ := data(t, out)["tunnels"].([]any)
	require.Len(t, list, 1, out["_raw"])
	b, _ := list[0].(map[string]any)["bytes"].(map[string]any)
	require.NotNil(t, b, out["_raw"])
	within(t, down, b["to_user"].(float64), "tunnel to_user")
	within(t, up, b["to_device"].(float64), "tunnel to_device")

	var totals map[string]any
	require.Eventually(t, func() bool {
		got, res := h.do(t, http.MethodGet, "/usage?granularity=hour&from="+time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339), alice, nil)
		if got != http.StatusOK {
			return false
		}
		totals = data(t, res)["totals"].(map[string]any)
		return totals["tunnel_bytes_to_user"].(float64) >= down && totals["upload_bytes_to_devices"].(float64) >= size &&
			totals["jobs_command"].(float64) >= 1 && totals["device_days"].(float64) >= 1
	}, 30*time.Second, 500*time.Millisecond, "usage arrives: %v", totals)
	within(t, down, totals["tunnel_bytes_to_user"].(float64), "usage tunnel_bytes_to_user")
	within(t, up, totals["tunnel_bytes_to_device"].(float64), "usage tunnel_bytes_to_device")
	within(t, size, totals["upload_bytes_to_devices"].(float64), "usage upload_bytes_to_devices")
	assert.EqualValues(t, 1, totals["jobs_command"])
	assert.EqualValues(t, 1, totals["jobs_run"])
	assert.EqualValues(t, 1, totals["devices_peak"])
	assert.EqualValues(t, 1, totals["device_days"])
	assert.Greater(t, totals["api_bytes_out"].(float64), 0.0)
	code, out = h.do(t, http.MethodGet, "/usage", alice, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	top := data(t, out)["top_devices"].([]any)
	require.NotEmpty(t, top, out["_raw"])
	assert.Equal(t, peerClientID, top[0].(map[string]any)["client_id"])

	// organization b sees none of it; a non-administrator of a sees nothing
	code, out = h.do(t, http.MethodGet, "/usage", bob, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.EqualValues(t, 0, data(t, out)["totals"].(map[string]any)["transfer_bytes"])
	code, out = h.do(t, http.MethodGet, "/usage", h.login(t, "carl"), nil)
	assert.Equal(t, http.StatusForbidden, code, out["_raw"])
	code, out = h.do(t, http.MethodGet, "/usage?granularity=week", alice, nil)
	assert.Equal(t, http.StatusBadRequest, code, out["_raw"])

	// operators: the summary across organizations, heaviest first, and one organization's report
	code, out = h.do(t, http.MethodGet, "/operator/usage", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	orgs := data(t, out)["organizations"].([]any)
	require.NotEmpty(t, orgs)
	first := orgs[0].(map[string]any)
	assert.Equal(t, "a", first["id"], out["_raw"])
	within(t, up+down+size, first["totals"].(map[string]any)["transfer_bytes"].(float64), "operator transfer_bytes")
	ids := map[string]bool{}
	for _, o := range orgs {
		ids[o.(map[string]any)["id"].(string)] = true
	}
	assert.True(t, ids["b"], "organizations without usage are listed")
	assert.False(t, ids["_system"])
	code, out = h.do(t, http.MethodGet, "/operator/usage?organization=b", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Len(t, data(t, out)["organizations"], 1)
	code, out = h.do(t, http.MethodGet, "/operator/organizations/a/usage?granularity=hour", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	within(t, down, data(t, out)["totals"].(map[string]any)["tunnel_bytes_to_user"].(float64), "operator org report")
	code, _ = h.do(t, http.MethodGet, "/operator/organizations/nope/usage", op, nil)
	assert.Equal(t, http.StatusNotFound, code)
	code, _ = h.do(t, http.MethodGet, "/operator/usage", alice, nil)
	assert.Equal(t, http.StatusNotFound, code, "organizations have no operator API")
	code, _ = h.do(t, http.MethodGet, "/usage", op, nil)
	assert.NotEqual(t, http.StatusOK, code, "the operators' tenant has no organization usage")
}

// Without [metering] a Postgres node has no usage routes and reports no usage capability.
func TestNodeWithoutMeteringHasNoUsageAPI(t *testing.T) {
	h := startNode(t)
	alice, op := h.login(t, "alice"), h.login(t, "op")
	_, ok := capabilities(t, h, alice)["usage"]
	assert.False(t, ok)
	code, _ := h.do(t, http.MethodGet, "/usage", alice, nil)
	assert.Equal(t, http.StatusNotFound, code)
	code, _ = h.do(t, http.MethodGet, "/operator/usage", op, nil)
	assert.Equal(t, http.StatusNotFound, code)
}
