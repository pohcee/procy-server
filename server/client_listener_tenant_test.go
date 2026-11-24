package chserver

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/auditlog"
	auditlogconfig "github.com/openrport/openrport/server/auditlog/config"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/share/comm"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/tenant"
	"github.com/openrport/openrport/share/ws"
)

type notifiedMeasurement struct {
	tenant tenant.ID
	m      models.Measurement
}

type recordingMeasurementQueue struct {
	got []notifiedMeasurement
}

func (q *recordingMeasurementQueue) Notify(ctx context.Context, m models.Measurement) bool {
	id, _ := tenant.FromContext(ctx)
	q.got = append(q.got, notifiedMeasurement{tenant: id, m: m})
	return true
}

func (q *recordingMeasurementQueue) Close() error { return nil }

func TestMeasurementFromClientIsQueuedWithTheClientsTenant(t *testing.T) {
	queue := &recordingMeasurementQueue{}
	cfg := &chconfig.Config{}
	cfg.Monitoring.Enabled = true
	cfg.Server.MaxRequestBytesClient = 1 << 20
	cl := &ClientListener{server: &Server{config: cfg, monitoringQueue: queue}}

	reqs := make(chan *ssh.Request, 1)
	reqs <- &ssh.Request{Type: comm.RequestTypeSaveMeasurement, Payload: []byte(`{"cpu_usage_percent": 12}`)}
	close(reqs)

	clientCtx := tenant.WithID(context.Background(), "a")
	clientLog := logger.NewDynamicLogger("client", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug, false, false)
	cl.handleSSHRequests(clientCtx, clientLog, "c1", reqs)

	require.Len(t, queue.got, 1)
	assert.Equal(t, tenant.ID("a"), queue.got[0].tenant)
	assert.Equal(t, "c1", queue.got[0].m.ClientID)
	assert.Equal(t, 12.0, queue.got[0].m.CPUUsagePercent)
}

type noClients struct{}

func (noClients) GetByID(context.Context, string) (*clientdata.Client, error) { return nil, nil }

func TestCmdResultAuditEntryIsSavedForTheClientsTenant(t *testing.T) {
	jp := makeJobsProvider(t, DataSourceOptions, testLog)
	t.Cleanup(func() { jp.Close() })
	require.NoError(t, jp.CreateJob(tenant.WithID(context.Background(), "a"), &models.Job{JID: "j1", ClientID: "c1", Status: models.JobStatusRunning, StartedAt: time.Now()}))

	al := newTestAuditLog(t)
	t.Cleanup(func() { al.Close() })

	cfg := &chconfig.Config{}
	cfg.Server.MaxRequestBytesClient = 1 << 20
	cl := &ClientListener{logger: testLog, server: &Server{config: cfg, jobProvider: jp, uiJobWebSockets: ws.NewWebSocketCache(), auditLog: al}}

	reqs := make(chan *ssh.Request, 1)
	reqs <- &ssh.Request{Type: comm.RequestTypeCmdResult, Payload: []byte(`{"jid":"j1","client_id":"c1","status":"successful"}`)}
	close(reqs)
	clientLog := logger.NewDynamicLogger("client", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug, false, false)
	cl.handleSSHRequests(tenant.WithID(context.Background(), "a"), clientLog, "c1", reqs)

	admin := &users.User{Username: "admin", Groups: []string{users.Administrators}}
	list := func(id tenant.ID) []*auditlog.Entry {
		r := httptest.NewRequest("GET", "/auditlog", nil).WithContext(tenant.WithID(context.Background(), id))
		res, err := al.List(r, admin)
		require.NoError(t, err)
		return res.Data.([]*auditlog.Entry)
	}
	entries := list("a")
	require.Len(t, entries, 1)
	assert.Equal(t, "j1", entries[0].ID)
	assert.Equal(t, "c1", entries[0].ClientID)
	assert.Empty(t, list(tenant.Default))
}

func TestUploadAuditEntryIsSavedForTheClientsTenant(t *testing.T) {
	al := newTestAuditLog(t)
	t.Cleanup(func() { al.Close() })
	api := &APIListener{Logger: testLog, Server: &Server{auditLog: al}}

	client := &clientdata.Client{}
	client.SetID("c1")
	client.Context = tenant.WithID(context.Background(), "a")
	results := make(chan *uploadResult, 1)
	results <- &uploadResult{client: client, resp: &models.UploadResponse{}}
	close(results)
	api.consumeUploadResults(results, &UploadRequest{UploadedFile: &models.UploadedFile{ID: "u1"}})

	admin := &users.User{Username: "admin", Groups: []string{users.Administrators}}
	r := httptest.NewRequest("GET", "/auditlog", nil).WithContext(tenant.WithID(context.Background(), "a"))
	res, err := al.List(r, admin)
	require.NoError(t, err)
	entries := res.Data.([]*auditlog.Entry)
	require.Len(t, entries, 1)
	assert.Equal(t, "u1", entries[0].ID)
}

// newTestAuditLog is an enabled audit log with monthly rotation in a temp dir, run in the default tenant's context.
func newTestAuditLog(t *testing.T) *auditlog.AuditLog {
	t.Helper()
	ctx := tenant.WithID(context.Background(), tenant.Default)
	cfg := auditlogconfig.Config{Enable: true, Rotation: auditlogconfig.RotationMonthly}
	p, err := auditlog.NewRotationProvider(ctx, testLog, cfg.RotationPeriod(), t.TempDir(), DataSourceOptions)
	require.NoError(t, err)
	al, err := auditlog.New(ctx, testLog, noClients{}, cfg, p)
	require.NoError(t, err)
	return al
}
