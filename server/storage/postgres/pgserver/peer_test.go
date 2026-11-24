package pgserver

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	chclient "github.com/openrport/openrport/client"
	chserver "github.com/openrport/openrport/server"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/clientsauth"
	"github.com/openrport/openrport/server/peer"
	pgclientauth "github.com/openrport/openrport/server/storage/postgres/clientauth"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	"github.com/openrport/openrport/share/clientconfig"
	"github.com/openrport/openrport/share/files"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

const (
	peerTD       = "test.cluster"
	peerAdmin    = "admin"
	peerClientID = "dev1"
	peerAuthID   = "dev1-auth"
	peerAuthPass = "dev1-client-password-0"
)

// peerCerts issues a CA and node certificates into a directory and returns the [cluster] lines for each.
type peerCerts struct {
	dir           string
	caCert, caKey []byte
	caFile        string
}

func newPeerCerts(t *testing.T) *peerCerts {
	t.Helper()
	dir := t.TempDir()
	caCert, caKey, err := peer.NewCA("test cluster CA", time.Hour)
	require.NoError(t, err)
	c := &peerCerts{dir: dir, caCert: caCert, caKey: caKey, caFile: filepath.Join(dir, "ca.crt")}
	require.NoError(t, os.WriteFile(c.caFile, caCert, 0o600))
	return c
}

// files issues a certificate for node with roles and returns its cert and key files.
func (c *peerCerts) files(t *testing.T, node string, roles ...peer.Role) (certFile, keyFile string) {
	t.Helper()
	cert, key, err := peer.IssueNodeCert(c.caCert, c.caKey, peer.NodeCertRequest{TrustDomain: peerTD, NodeID: node, Roles: roles, Validity: time.Hour})
	require.NoError(t, err)
	name := filepath.Join(c.dir, fmt.Sprintf("%s-%d", node, len(roles)))
	require.NoError(t, os.WriteFile(name+".crt", cert, 0o600))
	require.NoError(t, os.WriteFile(name+".key", key, 0o600))
	return name + ".crt", name + ".key"
}

func (c *peerCerts) cluster(t *testing.T, node, peerAddr string, roles ...peer.Role) string {
	t.Helper()
	cert, key := c.files(t, node, roles...)
	s := fmt.Sprintf("  trust_domain = %q\n  ca_file = %q\n  cert_file = %q\n  key_file = %q\n", peerTD, c.caFile, cert, key)
	if peerAddr != "" {
		s += fmt.Sprintf("  peer_address = %q\n", peerAddr)
	}
	return s
}

// connectClient connects a real rport client to the client listener at addr.
func connectClient(t *testing.T, addr string) {
	t.Helper()
	dataDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "scripts"), 0o700))
	cfg := &chclient.ClientConfigHolder{Config: &clientconfig.Config{
		Client: clientconfig.ClientConfig{
			Server:  "http://" + addr,
			Auth:    peerAuthID + ":" + peerAuthPass,
			ID:      peerClientID,
			Name:    peerClientID,
			DataDir: dataDir,
		},
		RemoteCommands:      clientconfig.CommandsConfig{Enabled: true, SendBackLimit: 4096, Allow: []string{".*"}, Order: [2]string{"allow", "deny"}},
		FileReceptionConfig: clientconfig.FileReceptionConfig{Enabled: true},
		Logging:             clientconfig.LogConfig{LogLevel: logger.LogLevelError, LogOutput: logger.LogOutput{File: os.Stderr}},
		Connection:          clientconfig.ConnectionConfig{MaxRetryCount: -1},
	}}
	require.NoError(t, cfg.ParseAndValidate(true))
	c, err := chclient.NewClient(cfg, files.NewFileSystem())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, c.Start(ctx))
	t.Cleanup(func() {
		cancel()
		_ = c.Close()
	})
}

// clientState returns the connection state node h shows for the client ("" when it doesn't know the client).
func clientState(t *testing.T, h *nodeFixture, token string) string {
	t.Helper()
	code, out := h.do(t, http.MethodGet, "/clients/"+peerClientID, token, nil)
	if code != http.StatusOK {
		return ""
	}
	data, _ := out["data"].(map[string]any)
	s, _ := data["connection_state"].(string)
	return s
}

func echoServer(t *testing.T) string {
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
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return l.Addr().String()
}

// TestPeerDispatchEndToEnd runs an api process A and a device process B (storage role "all", no API) on one
// database with peer calls over mTLS, a real client connected to B, and drives the client through A's API: a command
// with live output on the UI websocket and the final result, an upload, a tunnel that carries traffic. A second api
// process started after the client connected shows it connected, as A does.
func TestPeerDispatchEndToEnd(t *testing.T) {
	d := pgtest.New(t)
	ctx := tenant.WithID(context.Background(), tenant.Default)
	require.NoError(t, AddUser(ctx, d.App, tenant.Default, peerAdmin, testPassword, []string{"Administrators"}))
	_, err := pgclientauth.New(d.App).Add(ctx, &clientsauth.ClientAuth{ID: peerAuthID, Password: peerAuthPass})
	require.NoError(t, err)

	certs := newPeerCerts(t)
	listenB, peerB := freeAddr(t), freeAddr(t)
	a := startNodeOn(t, d, nodeSettings{nodeID: "node-a", cluster: certs.cluster(t, "node-a", "", peer.RoleAPI)})
	startNodeOn(t, d, nodeSettings{
		nodeID: "node-b", role: "all", apiOff: true, listen: listenB,
		server:  `  tunnel_host = "node-b.example.test"` + "\n" + `  used_ports = ["20000-30000"]`,
		cluster: certs.cluster(t, "node-b", peerB, peer.RoleAPI, peer.RoleDevice),
	})
	token := a.login(t, peerAdmin)

	connectClient(t, listenB)
	// A started before the client connected: it learns about it from the database
	require.Eventually(t, func() bool { return clientState(t, a, token) == "connected" }, 20*time.Second, 100*time.Millisecond,
		"node A shows the client connected to node B")

	// a command through A, on the UI websocket: live output, then the result
	wsURL := strings.Replace(a.base, "http://", "ws://", 1) + "/ws/commands?access_token=" + token
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	if resp != nil {
		resp.Body.Close()
	}
	defer conn.Close()
	require.NoError(t, conn.WriteJSON(map[string]any{
		"client_ids": []string{peerClientID}, "command": "echo live-one; sleep 1; echo live-two", "timeout_sec": 30,
	}))
	var outputs []string
	var final map[string]any
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(30*time.Second)))
	for final == nil {
		_, msg, err := conn.ReadMessage()
		require.NoError(t, err, "outputs so far: %v", outputs)
		var m map[string]any
		require.NoError(t, json.Unmarshal(msg, &m), string(msg))
		if _, ok := m["status"]; ok {
			final = m
			break
		}
		outputs = append(outputs, string(msg))
	}
	assert.NotEmpty(t, outputs, "live output arrived before the result")
	assert.Contains(t, strings.Join(outputs, ""), "live-one")
	assert.Equal(t, "successful", final["status"], "%v", final)
	assert.Contains(t, fmt.Sprint(final["result"]), "live-two")
	t.Logf("websocket: %d output messages, then status %v", len(outputs), final["status"])

	// the same through the REST API; the result is stored by B and read by A
	code, out := a.do(t, http.MethodPost, "/clients/"+peerClientID+"/commands", token, map[string]any{"command": "echo rest-call", "timeout_sec": 10})
	require.Equal(t, http.StatusOK, code, out["_raw"])
	jid := out["data"].(map[string]any)["jid"].(string)
	require.Eventually(t, func() bool {
		got, res := a.do(t, http.MethodGet, "/clients/"+peerClientID+"/commands/"+jid, token, nil)
		if got != http.StatusOK {
			return false
		}
		job := res["data"].(map[string]any)
		return job["status"] == "successful" && strings.Contains(fmt.Sprint(job["result"]), "rest-call")
	}, 30*time.Second, 200*time.Millisecond, "the command's result is readable through A")

	// an upload through A arrives on the client
	dest := filepath.Join(t.TempDir(), "uploaded.txt")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	require.NoError(t, mw.WriteField("client_id", peerClientID))
	require.NoError(t, mw.WriteField("dest", dest))
	fw, err := mw.CreateFormFile("upload", "uploaded.txt")
	require.NoError(t, err)
	_, err = fw.Write([]byte("uploaded through the peer API"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	req, err := http.NewRequest(http.MethodPost, a.base+"/files", &body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	uresp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	raw, _ := io.ReadAll(uresp.Body)
	uresp.Body.Close()
	require.Equal(t, http.StatusOK, uresp.StatusCode, string(raw))
	require.Eventually(t, func() bool {
		b, err := os.ReadFile(dest)
		return err == nil && string(b) == "uploaded through the peer API"
	}, 20*time.Second, 100*time.Millisecond, "the uploaded file arrives on the client")

	// a tunnel opened through A carries traffic through B
	target := echoServer(t)
	code, out = a.do(t, http.MethodPut, "/clients/"+peerClientID+"/tunnels?remote="+target+"&scheme=other", token, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	tun := out["data"].(map[string]any)
	assert.Equal(t, "node-b.example.test", tun["tunnel_host"], "the link names the node holding the tunnel")
	tc, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(tun["lport"])), 5*time.Second)
	require.NoError(t, err)
	_, err = tc.Write([]byte("ping"))
	require.NoError(t, err)
	got := make([]byte, 4)
	require.NoError(t, tc.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = io.ReadFull(tc, got)
	require.NoError(t, err)
	assert.Equal(t, "ping", string(got))
	tc.Close()
	// and is closed through A (its record reached A)
	code, out = a.do(t, http.MethodDelete, fmt.Sprintf("/clients/%s/tunnels/%v", peerClientID, tun["id"]), token, nil)
	assert.Equal(t, http.StatusNoContent, code, out["_raw"])

	// an api process started after the client connected shows it connected
	c := startNodeOn(t, d, nodeSettings{nodeID: "node-c", cluster: certs.cluster(t, "node-c", "", peer.RoleAPI)})
	assert.Equal(t, "connected", clientState(t, c, c.login(t, peerAdmin)))

	// a device certificate can't call B's peer API; neither can a caller without a certificate
	cert, key := certs.files(t, "node-x", peer.RoleDevice)
	pair, err := tls.LoadX509KeyPair(cert, key)
	require.NoError(t, err)
	for name, cfg := range map[string]*tls.Config{
		"device certificate": {Certificates: []tls.Certificate{pair}, InsecureSkipVerify: true}, //nolint:gosec // test
		"no certificate":     {InsecureSkipVerify: true},                                        //nolint:gosec // test
	} {
		hc := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
		r, err := hc.Post("https://"+peerB+"/peer/v1/call", "application/json",
			strings.NewReader(`{"tenant":"default","client_id":"dev1","op":"refresh_updates_status"}`))
		if err == nil {
			r.Body.Close()
			assert.Equal(t, http.StatusForbidden, r.StatusCode, name)
		}
	}
}

// Without peer calls an api node still refuses work on clients as not connected (as in phase 2).
func TestAPINodeWithoutPeerCallsRefusesWork(t *testing.T) {
	d := pgtest.New(t)
	ctx := tenant.WithID(context.Background(), tenant.Default)
	require.NoError(t, AddUser(ctx, d.App, tenant.Default, peerAdmin, testPassword, []string{"Administrators"}))
	_, err := pgclientauth.New(d.App).Add(ctx, &clientsauth.ClientAuth{ID: peerAuthID, Password: peerAuthPass})
	require.NoError(t, err)
	listenB := freeAddr(t)
	a := startNodeOn(t, d, nodeSettings{nodeID: "node-a"})
	startNodeOn(t, d, nodeSettings{nodeID: "node-b", role: "all", apiOff: true, listen: listenB})
	token := a.login(t, peerAdmin)
	connectClient(t, listenB)
	require.Eventually(t, func() bool { return clientState(t, a, token) == "connected" }, 20*time.Second, 100*time.Millisecond)
	code, out := a.do(t, http.MethodPost, "/clients/"+peerClientID+"/commands", token, map[string]any{"command": "echo x", "timeout_sec": 10})
	assert.NotEqual(t, http.StatusOK, code, out["_raw"])
}

// With peer calls every client is reachable from every node: several nodes accept client connections, and api nodes
// claim scheduled runs. Certificates must match the node and carry the roles of its storage role.
func TestPeerCallsLiftOneListenerAndRunLimits(t *testing.T) {
	d := pgtest.New(t)
	certs := newPeerCerts(t)
	both := []peer.Role{peer.RoleAPI, peer.RoleDevice}
	startNodeOn(t, d, nodeSettings{nodeID: "node-a", role: chconfig.StorageRoleAll, apiOff: true, listen: freeAddr(t),
		cluster: certs.cluster(t, "node-a", freeAddr(t), both...)})
	startNodeOn(t, d, nodeSettings{nodeID: "node-b", role: chconfig.StorageRoleAll, apiOff: true, listen: freeAddr(t),
		cluster: certs.cluster(t, "node-b", freeAddr(t), both...)})

	open := func(s nodeSettings) (*chserver.ServerOpts, error) {
		cfg := loadConfig(t, nodeConf(t, d.AppURL, freeAddr(t), s))
		opts, release, err := NodeOpts(context.Background(), cfg, d.App, &chserver.ServerOpts{FilesAPI: files.NewFileSystem()}, io.Discard)
		if err == nil {
			t.Cleanup(release)
		}
		return opts, err
	}
	opts, err := open(nodeSettings{nodeID: "node-c", cluster: certs.cluster(t, "node-c", "", peer.RoleAPI)})
	require.NoError(t, err)
	coord := opts.Cluster.(*Coordinator)
	assert.False(t, coord.noRuns, "an api node with peer calls claims scheduled runs")
	assert.IsType(t, &peer.Dispatcher{}, opts.Dispatcher)

	_, err = open(nodeSettings{nodeID: "node-d", cluster: certs.cluster(t, "node-x", "", peer.RoleAPI)})
	assert.ErrorContains(t, err, `cert_file is for node "node-x", but node_id is "node-d"`)
	_, err = open(nodeSettings{nodeID: "node-e", role: chconfig.StorageRoleAll, cluster: certs.cluster(t, "node-e", freeAddr(t), peer.RoleDevice)})
	assert.ErrorContains(t, err, `cert_file has no "api" role`)
}
