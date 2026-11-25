package chserver

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/ptr"
	"github.com/openrport/openrport/share/tenant"
	"github.com/openrport/openrport/share/ws"
)

func TestHandleOutputChannel(t *testing.T) {
	log := logger.NewLogger("client-listener-test", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug)
	cl := &ClientListener{server: &listenerServices{uiJobWebSockets: newUIStreams()}}
	mockConn := &connMock{}
	ws := ws.NewConcurrentWebSocket(mockConn, log)
	cl.server.uiJobWebSockets.Set("test-jid", ws)

	testCases := []struct {
		Name     string
		Job      models.Job
		Type     string
		Expected interface{}
	}{
		{
			Name: "no ws",
			Job: models.Job{
				JID: "other-jid",
			},
			Type:     models.ChannelStdout,
			Expected: nil,
		},
		{
			Name: "jid",
			Job: models.Job{
				JID: "test-jid",
			},
			Type: models.ChannelStdout,
			Expected: outputChannelData{
				JID: "test-jid",
				Result: &models.JobResult{
					StdOut: "test-output",
				},
			},
		},
		{
			Name: "multi job jid",
			Job: models.Job{
				MultiJobID: ptr.String("test-jid"),
				JID:        "job-jid",
			},
			Type: models.ChannelStdout,
			Expected: outputChannelData{
				JID: "job-jid",
				Result: &models.JobResult{
					StdOut: "test-output",
				},
			},
		},
		{
			Name: "stderr",
			Job: models.Job{
				JID: "test-jid",
			},
			Type: models.ChannelStderr,
			Expected: outputChannelData{
				JID: "test-jid",
				Result: &models.JobResult{
					StdErr: "test-output",
				},
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			// t.Parallel()

			jobData, err := json.Marshal(tc.Job)
			require.NoError(t, err)

			reader, writer := io.Pipe()

			wg := sync.WaitGroup{}
			wg.Add(1)

			go func() {
				defer wg.Done()
				err := cl.handleOutputChannel(tc.Type, jobData, log, reader)
				require.NoError(t, err)
			}()

			_, err = writer.Write([]byte("test-output"))
			require.NoError(t, err)

			writer.Close()

			wg.Wait()
			assert.Equal(t, tc.Expected, mockConn.LastWrite)
		})
	}
}

type connMock struct {
	ws.Conn

	LastWrite interface{}
}

func (c *connMock) WriteJSON(data interface{}) error {
	c.LastWrite = data
	return nil
}

// orderConn records the writes to a UI websocket in order.
type orderConn struct {
	ws.Conn
	mu     sync.Mutex
	writes []string
}

func (c *orderConn) WriteJSON(v interface{}) error {
	b, _ := json.Marshal(v)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, "output:"+string(b))
	return nil
}

func (c *orderConn) WriteMessage(t int, b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t == websocket.CloseMessage {
		c.writes = append(c.writes, "close")
		return nil
	}
	c.writes = append(c.writes, "final:"+string(b))
	return nil
}

func (c *orderConn) Close() error { return nil }

func (c *orderConn) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.writes...)
}

// A client sends the result of a fast command (echo) while its output is still on the way in the output channel
// (handled by another goroutine): the result waits for the output, so the UI gets the output, then the result,
// then the close frame.
func TestCmdResultWaitsForTheJobsOutput(t *testing.T) {
	jp := makeJobsProvider(t, DataSourceOptions, testLog)
	t.Cleanup(func() { jp.Close() })
	cl := &ClientListener{logger: testLog, server: &listenerServices{
		config:          &chconfig.Config{Server: chconfig.ServerConfig{MaxRequestBytesClient: 1024 * 1024}},
		jobProvider:     jp,
		uiJobWebSockets: newUIStreams(),
	}}
	conn := &orderConn{}
	cl.server.uiJobWebSockets.Set("j1", ws.NewConcurrentWebSocket(conn, testLog))
	jobData := []byte(`{"jid":"j1","client_id":"c1"}`)

	// the output channel is accepted when the client opens it, before the command runs
	end := cl.outputOpened(jobData)
	reader, writer := io.Pipe()
	go func() {
		defer end()
		assert.NoError(t, cl.handleOutputChannel(models.ChannelStdout, jobData, testLog, reader))
	}()

	saved := make(chan struct{})
	go func() {
		defer close(saved)
		_, err := cl.saveCmdResult(tenant.WithID(context.Background(), tenant.Default),
			[]byte(`{"jid":"j1","client_id":"c1","status":"successful","started_at":"2026-01-01T00:00:00Z"}`))
		assert.NoError(t, err)
	}()
	time.Sleep(100 * time.Millisecond)
	assert.Empty(t, conn.all(), "the result waits for the output")

	_, err := writer.Write([]byte("instant\n"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	select {
	case <-saved:
	case <-time.After(5 * time.Second):
		t.Fatal("the result was not saved")
	}
	w := conn.all()
	require.Len(t, w, 3)
	assert.Contains(t, w[0], "instant")
	assert.Contains(t, w[1], "final:")
	assert.Equal(t, "close", w[2])
}

// The wait for the output is bounded: a channel that never ends doesn't hold the result.
func TestCmdResultOutputWaitIsBounded(t *testing.T) {
	jp := makeJobsProvider(t, DataSourceOptions, testLog)
	t.Cleanup(func() { jp.Close() })
	cl := &ClientListener{logger: testLog, outputWait: 50 * time.Millisecond, server: &listenerServices{
		config:          &chconfig.Config{Server: chconfig.ServerConfig{MaxRequestBytesClient: 1024 * 1024}},
		jobProvider:     jp,
		uiJobWebSockets: newUIStreams(),
	}}
	conn := &orderConn{}
	cl.server.uiJobWebSockets.Set("j1", ws.NewConcurrentWebSocket(conn, testLog))
	end := cl.outputOpened([]byte(`{"jid":"j1","client_id":"c1"}`))
	defer end()
	_, err := cl.saveCmdResult(tenant.WithID(context.Background(), tenant.Default),
		[]byte(`{"jid":"j1","client_id":"c1","status":"successful","started_at":"2026-01-01T00:00:00Z"}`))
	require.NoError(t, err)
	assert.Contains(t, conn.all()[0], "final:")
}
