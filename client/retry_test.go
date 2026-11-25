package chclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v3/host"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/client/system"
	"github.com/openrport/openrport/share/clientconfig"
	"github.com/openrport/openrport/share/test"
)

func retryTestClient(maxRetry time.Duration) *Client {
	return &Client{configHolder: &ClientConfigHolder{Config: &clientconfig.Config{
		Connection: clientconfig.ConnectionConfig{MaxRetryInterval: maxRetry},
	}}}
}

// Waits between connection attempts grow up to [connection] max_retry_interval and never beyond it, also after the
// server refused the connection request.
func TestRetryDelayRespectsMaxRetryInterval(t *testing.T) {
	for _, maxRetry := range []time.Duration{time.Second, 10 * time.Second, 5 * time.Minute} {
		c := retryTestClient(maxRetry)
		b := c.newConnectionBackoff()
		for attempt := 0; attempt < 30; attempt++ {
			for _, err := range []error{errors.New("dial tcp: connection refused"), errors.New("client is already connected or previous session was not properly closed")} {
				d := c.retryDelay(b, err)
				assert.LessOrEqual(t, d, maxRetry, "attempt %d after %q", attempt, err)
				assert.Positive(t, d)
			}
		}
	}
}

// A refusal by the client's organization (suspended, client limit reached) waits max_retry_interval, or a minute
// when that is shorter: the server can't take the client until an operator acts, so retrying sooner only costs it a
// handshake and a database round trip per attempt.
func TestRetryDelayAfterRefusalByTheOrganization(t *testing.T) {
	for _, tc := range []struct {
		maxRetry, atLeast, atMost time.Duration
	}{
		{10 * time.Second, 45 * time.Second, time.Minute},
		{5 * time.Minute, 225 * time.Second, 5 * time.Minute},
	} {
		c := retryTestClient(tc.maxRetry)
		b := c.newConnectionBackoff()
		for _, msg := range []string{
			"connection refused: the organization's limit of 1 clients is reached",
			"connection refused: the organization of this client is suspended",
		} {
			for i := 0; i < 20; i++ {
				d := c.retryDelay(b, errors.New(msg))
				assert.GreaterOrEqual(t, d, tc.atLeast, msg)
				assert.LessOrEqual(t, d, tc.atMost, msg)
			}
		}
		// the server failing to check the organization is an ordinary error
		assert.LessOrEqual(t, c.retryDelay(b, errors.New("connection refused: the server could not check the organization of this client; try again later")), tc.maxRetry)
	}
}

// A connection the server refused is closed: the next attempt makes a new one.
func TestRefusedConnectionIsClosed(t *testing.T) {
	config := &ClientConfigHolder{Config: &clientconfig.Config{Client: clientconfig.ClientConfig{ID: "c1", Name: "c1"}}}
	c, err := NewClient(config, test.NewFileAPIMock())
	require.NoError(t, err)
	c.systemInfo = &system.MockSystemInfo{ReturnHostInfo: &host.InfoStat{}}
	conn := test.NewConnMock()
	conn.ReturnResponsePayload = []byte("connection refused: the organization's limit of 1 clients is reached")
	err = c.sendConnectionRequest(context.Background(), conn, time.Millisecond)
	assert.EqualError(t, err, "connection refused: the organization's limit of 1 clients is reached")
	assert.True(t, conn.Closed())
}
