package ws_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/ws"
)

func TestAutoClose(t *testing.T) {
	log := logger.NewLogger("websocket-test", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug)
	mockConn := &connMock{}

	socket := ws.NewConcurrentWebSocket(mockConn, log)
	socket.SetWritesBeforeClose(2)

	err := socket.WriteMessage(websocket.TextMessage, []byte("test"))
	require.NoError(t, err)

	assert.False(t, mockConn.Closed)

	err = socket.WriteNonFinalJSON(1)
	require.NoError(t, err)

	assert.False(t, mockConn.Closed)

	err = socket.WriteJSON(1)
	require.NoError(t, err)

	// a close frame is sent; the connection is closed after the UI's answer, or the grace period
	assert.Equal(t, []int{websocket.TextMessage, websocket.CloseMessage}, mockConn.types())
	socket.CloseNow()
	assert.True(t, mockConn.isClosed())
}

type connMock struct {
	ws.Conn

	mu     sync.Mutex
	Closed bool
	sent   []int
}

func (c *connMock) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Closed = true
	return nil
}

func (c *connMock) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Closed
}

func (c *connMock) types() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int(nil), c.sent...)
}

func (c *connMock) WriteMessage(t int, _ []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, t)
	return nil
}

func (c *connMock) WriteJSON(_ interface{}) error {
	return nil
}

// The last write of a stream ends it with a close frame (1000), not a dropped connection (1006), after every
// message: the UI sees all output and the result, then a clean close. Closing twice is not an error.
func TestStreamEndsWithACloseFrame(t *testing.T) {
	log := logger.NewLogger("websocket-test", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug)
	upgrader := websocket.Upgrader{}
	served := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(served)
		c, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		socket := ws.NewConcurrentWebSocket(c, log)
		socket.SetWritesBeforeClose(1)
		for i := 0; i < 50; i++ {
			assert.NoError(t, socket.WriteNonFinalJSON(map[string]int{"output": i}))
		}
		assert.NoError(t, socket.WriteMessage(websocket.TextMessage, []byte(`{"result":"done"}`)))
		assert.Error(t, socket.WriteNonFinalJSON("late"), "nothing is written after the close frame")
		// the UI answers the close; the server reads it and closes
		_, _, err = socket.ReadMessage()
		assert.True(t, websocket.IsCloseError(err, websocket.CloseNormalClosure), "%v", err)
		socket.CloseNow()
		assert.NoError(t, socket.Close())
	}))
	defer srv.Close()

	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	defer c.Close()
	n := 0
	for {
		_, msg, err := c.ReadMessage()
		if err != nil {
			assert.True(t, websocket.IsCloseError(err, websocket.CloseNormalClosure), "the stream ends with a close frame: %v", err)
			break
		}
		if n < 50 {
			assert.JSONEq(t, fmt.Sprintf(`{"output":%d}`, n), string(msg))
		} else {
			assert.JSONEq(t, `{"result":"done"}`, string(msg))
		}
		n++
	}
	assert.Equal(t, 51, n, "every output and the result arrive")
	<-served
}

// Without a reader for the UI's answer, the connection is closed after a grace period.
func TestCloseWithoutAnswerClosesAfterGrace(t *testing.T) {
	log := logger.NewLogger("websocket-test", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug)
	mockConn := &connMock{}
	socket := ws.NewConcurrentWebSocket(mockConn, log)
	socket.SetCloseGrace(50 * time.Millisecond)
	require.NoError(t, socket.Close())
	assert.Equal(t, []int{websocket.CloseMessage}, mockConn.types())
	assert.False(t, mockConn.isClosed())
	assert.Eventually(t, mockConn.isClosed, time.Second, 10*time.Millisecond)
	require.NoError(t, socket.Close())
	assert.Equal(t, []int{websocket.CloseMessage}, mockConn.types(), "one close frame")
}
