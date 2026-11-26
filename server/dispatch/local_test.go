package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	errors2 "github.com/pohcee/procy-server/server/api/errors"
	"github.com/pohcee/procy-server/server/billing"
	"github.com/pohcee/procy-server/server/clients"
	"github.com/pohcee/procy-server/server/clients/clientdata"
	"github.com/pohcee/procy-server/share/clientconfig"
	"github.com/pohcee/procy-server/share/comm"
	errors3 "github.com/pohcee/procy-server/share/errors"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/models"
	"github.com/pohcee/procy-server/share/tenant"
	"github.com/pohcee/procy-server/share/test"
)

var testLog = logger.NewLogger("dispatch-test", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug)

// recordingConn answers every request with reply and records what was sent; during the send it
// calls onSend, if set.
type recordingConn struct {
	*test.ConnMock
	reply   []byte
	onSend  func()
	types   []string
	payload []byte
}

func (c *recordingConn) SendRequest(name string, _ bool, payload []byte) (bool, []byte, error) {
	c.types = append(c.types, name)
	c.payload = payload
	if c.onSend != nil {
		c.onSend()
	}
	return true, c.reply, nil
}

func defaultCtx() context.Context {
	return tenant.WithID(context.Background(), tenant.Default)
}

// newLocal returns a Local over a client service holding the given clients (default tenant).
func newLocal(t *testing.T, cls ...*clientdata.Client) (*Local, *FileGrants) {
	t.Helper()
	repo := clients.NewClientRepository(cls, nil, testLog)
	grants := NewFileGrants()
	return NewLocal(clients.NewClientService(nil, nil, repo, testLog, nil), grants, testLog), grants
}

func TestLocalRunJobSendsRunCmd(t *testing.T) {
	conn := &recordingConn{ConnMock: test.NewConnMock(), reply: []byte(`{"pid":42}`)}
	cl := clients.New(t).ID("c1").Logger(testLog).Connection(conn).Build()
	d, _ := newLocal(t, cl)

	job := &models.Job{JID: "j1", ClientID: "c1", Command: "date"}
	resp, err := d.RunJob(defaultCtx(), "c1", job)
	require.NoError(t, err)

	assert.Equal(t, 42, resp.Pid)
	assert.Equal(t, []string{comm.RequestTypeRunCmd}, conn.types)
	var sent models.Job
	require.NoError(t, json.Unmarshal(conn.payload, &sent))
	assert.Equal(t, "j1", sent.JID)
	assert.Equal(t, "date", sent.Command)
}

func TestLocalUnknownOrUnconnectedClient(t *testing.T) {
	noConn := clients.New(t).ID("no-conn").Logger(testLog).Build()
	noConn.SetConnection(nil)
	d, _ := newLocal(t, noConn)
	ctx := defaultCtx()

	for _, id := range []string{"unknown", "no-conn"} {
		t.Run(id, func(t *testing.T) {
			calls := map[string]error{}
			_, calls["RunJob"] = d.RunJob(ctx, id, &models.Job{})
			_, calls["Upload"] = d.Upload(ctx, id, &models.UploadedFile{})
			_, calls["IsTunnelAllowed"] = d.IsTunnelAllowed(ctx, id, "127.0.0.1:22")
			_, calls["CheckPort"] = d.CheckPort(ctx, id, &comm.CheckPortRequest{HostPort: "127.0.0.1:22"})
			_, calls["StartTunnels"] = d.StartTunnels(ctx, id, nil)
			if id == "unknown" {
				calls["CloseTunnel"] = d.CloseTunnel(ctx, id, "1", false)
				calls["SetTunnelACL"] = d.SetTunnelACL(ctx, id, "1", nil)
			}
			calls["RefreshUpdatesStatus"] = d.RefreshUpdatesStatus(ctx, id)
			calls["UpdateAttributes"] = d.UpdateAttributes(ctx, id, models.Attributes{})
			for method, err := range calls {
				assert.ErrorIs(t, err, ErrClientNotConnected, method)
			}
		})
	}
}

func TestLocalTunnelLookupNeedsNoConnection(t *testing.T) {
	cl := clients.New(t).ID("c1").Logger(testLog).Build()
	cl.SetConnection(nil)
	d, _ := newLocal(t, cl)

	// the server's end of a tunnel is found without the client's connection
	assert.ErrorIs(t, d.SetTunnelACL(defaultCtx(), "c1", "nope", nil), ErrTunnelNotFound)
	assert.ErrorIs(t, d.CloseTunnel(defaultCtx(), "c1", "nope", false), ErrTunnelNotFound)
}

func TestLocalOtherTenantIsNotReachable(t *testing.T) {
	conn := &recordingConn{ConnMock: test.NewConnMock(), reply: []byte(`{}`)}
	cl := clients.New(t).ID("c1").Logger(testLog).Connection(conn).Build()
	d, _ := newLocal(t, cl)

	_, err := d.RunJob(tenant.WithID(context.Background(), "b"), "c1", &models.Job{})
	assert.ErrorIs(t, err, ErrClientNotConnected)
	assert.Empty(t, conn.types, "nothing is sent to another tenant's client")

	_, err = d.RunJob(context.Background(), "c1", &models.Job{})
	assert.ErrorIs(t, err, tenant.ErrMissing)
}

func TestLocalUploadGrantsFileOnlyDuringSend(t *testing.T) {
	file := &models.UploadedFile{ID: "id-1", SourceFilePath: "/data/filepush/id-1_rport_filepush"}
	conn := &recordingConn{ConnMock: test.NewConnMock(), reply: []byte(`{"status":"success"}`)}
	cl := clients.New(t).ID("c1").Logger(testLog).Connection(conn).Build()
	d, grants := newLocal(t, cl)

	var allowedDuringSend, otherConnAllowed bool
	conn.onSend = func() {
		allowedDuringSend = grants.Allowed(conn, file.SourceFilePath)
		otherConnAllowed = grants.Allowed(test.NewConnMock(), file.SourceFilePath)
	}

	resp, err := d.Upload(defaultCtx(), "c1", file)
	require.NoError(t, err)

	assert.Equal(t, "success", resp.Status)
	assert.Equal(t, []string{comm.RequestTypeUpload}, conn.types)
	assert.True(t, allowedDuringSend, "the client must be able to fetch the file while the request is in flight")
	assert.False(t, otherConnAllowed, "only the receiving client's connection")
	assert.False(t, grants.Allowed(conn, file.SourceFilePath), "the grant ends with the send")
}

func TestLocalUploadRefusedWhenClientDisablesFileReception(t *testing.T) {
	conn := &recordingConn{ConnMock: test.NewConnMock(), reply: []byte(`{}`)}
	cfg := &clientconfig.Config{FileReceptionConfig: clientconfig.FileReceptionConfig{Enabled: false}}
	cl := clients.New(t).ID("c1").Logger(testLog).Connection(conn).Config(cfg).Build()
	d, _ := newLocal(t, cl)

	_, err := d.Upload(defaultCtx(), "c1", &models.UploadedFile{SourceFilePath: "/x"})
	assert.ErrorIs(t, err, errors3.ErrUploadsDisabled)
	assert.Empty(t, conn.types)
}

func TestLocalCloseTunnelUnknownTunnel(t *testing.T) {
	conn := &recordingConn{ConnMock: test.NewConnMock(), reply: []byte(`{}`)}
	cl := clients.New(t).ID("c1").Logger(testLog).Connection(conn).Build()
	d, _ := newLocal(t, cl)

	assert.ErrorIs(t, d.CloseTunnel(defaultCtx(), "c1", "nope", true), ErrTunnelNotFound)
	assert.ErrorIs(t, d.SetTunnelACL(defaultCtx(), "c1", "nope", nil), ErrTunnelNotFound)
}

func TestLocalCheckPortAndTunnelAllowed(t *testing.T) {
	conn := &recordingConn{ConnMock: test.NewConnMock(), reply: []byte(`{"Open":true,"IsAllowed":true}`)}
	cl := clients.New(t).ID("c1").Logger(testLog).Connection(conn).Build()
	d, _ := newLocal(t, cl)

	resp, err := d.CheckPort(defaultCtx(), "c1", &comm.CheckPortRequest{HostPort: "127.0.0.1:22"})
	require.NoError(t, err)
	assert.True(t, resp.Open)

	allowed, err := d.IsTunnelAllowed(defaultCtx(), "c1", "127.0.0.1:22")
	require.NoError(t, err)
	assert.True(t, allowed)

	require.NoError(t, d.RefreshUpdatesStatus(defaultCtx(), "c1"))
	require.NoError(t, d.UpdateAttributes(defaultCtx(), "c1", models.Attributes{Tags: []string{"a"}}))
	assert.Equal(t, []string{
		comm.RequestTypeCheckPort,
		comm.RequestTypeCheckTunnelAllowed,
		comm.RequestTypeRefreshUpdatesStatus,
		comm.RequestTypeUpdateClientAttributes,
	}, conn.types)
}

type capGuard struct{}

func (capGuard) Allow(tenant.ID) error {
	return fmt.Errorf("%w: plan %q", billing.ErrTransferLimit, "free")
}

// A refusal at the transfer limit is the API's 403 ERR_CODE_TRANSFER_LIMIT (as the api's own guard), here and
// across a peer call, not a 500.
func TestLocalTransferLimitIsA403(t *testing.T) {
	conn := &recordingConn{ConnMock: test.NewConnMock(), reply: []byte(`{}`)}
	cl := clients.New(t).ID("c1").Logger(testLog).Connection(conn).Build()
	d, _ := newLocal(t, cl)
	d.SetTransferGuard(capGuard{})

	_, err := d.StartTunnels(defaultCtx(), "c1", []*models.Remote{{RemoteHost: "127.0.0.1", RemotePort: "22"}})
	var ae errors2.APIError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, http.StatusForbidden, ae.HTTPStatus)
	assert.Equal(t, billing.ErrCodeTransferLimit, ae.ErrCode)
	assert.Contains(t, ae.Message, "transfer limit reached")

	_, err = d.Upload(defaultCtx(), "c1", &models.UploadedFile{ID: "f"})
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, http.StatusForbidden, ae.HTTPStatus)
}
