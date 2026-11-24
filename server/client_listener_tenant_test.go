package chserver

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/share/comm"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/tenant"
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
