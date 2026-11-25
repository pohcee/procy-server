package ws

import (
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/share/logger"
)

type Conn interface {
	NextReader() (messageType int, r io.Reader, err error)
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	WriteJSON(v interface{}) error
	Close() error
}

// ConcurrentWebSocket serializes the writes to a UI websocket. A stream ends after a number of final writes
// (SetWritesBeforeClose; 0 or less: never) or Close with the closing handshake: a close frame (1000) is sent, nothing
// is written after it, and the connection is closed when the UI's answer was read (CloseNow) or after a grace
// period. Closing the connection right away instead would make the UI see 1006 and could drop the last frames.
type ConcurrentWebSocket struct {
	conn              Conn
	mu                sync.Mutex
	log               *logger.Logger
	writesBeforeClose int
	closeGrace        time.Duration
	closeSent         bool
	closed            bool
	closeTimer        *time.Timer
}

// DefaultCloseGrace is how long a closing websocket waits for the UI to answer its close frame.
const DefaultCloseGrace = 2 * time.Second

// ErrClosing is returned by writes after the close frame was sent.
var ErrClosing = errors.New("websocket is closing")

func NewConcurrentWebSocket(conn Conn, log *logger.Logger) *ConcurrentWebSocket {
	return &ConcurrentWebSocket{
		conn:              conn,
		log:               log,
		writesBeforeClose: 1,
		closeGrace:        DefaultCloseGrace,
	}
}

func (ws *ConcurrentWebSocket) ReadJSON(inboundMsg interface{}) error {
	_, r, err := ws.conn.NextReader()
	if err != nil {
		return err
	}
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	return dec.Decode(inboundMsg)
}

func (ws *ConcurrentWebSocket) ReadMessage() (messageType int, p []byte, err error) {
	return ws.conn.ReadMessage()
}

func (ws *ConcurrentWebSocket) WriteError(title string, err error) {
	var errMsg string
	if err != nil {
		errMsg = err.Error()
	}
	_ = ws.WriteJSON(api.NewErrAPIPayloadFromMessage("", title, errMsg))
}

// WriteJSON write json message to websocket, counting towards writes before close
func (ws *ConcurrentWebSocket) WriteJSON(jsonOutboundMsg interface{}) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	err := ws.writeJSONLocked(jsonOutboundMsg)
	ws.decLocked()
	return err
}

// WriteNonFinalJSON write json message to websocket, not counting towards writes before close
func (ws *ConcurrentWebSocket) WriteNonFinalJSON(jsonOutboundMsg interface{}) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.writeJSONLocked(jsonOutboundMsg)
}

func (ws *ConcurrentWebSocket) writeJSONLocked(v interface{}) error {
	if ws.closeSent || ws.closed {
		ws.log.Debugf("WS json write after close: dropped")
		return ErrClosing
	}
	err := ws.conn.WriteJSON(v)
	if err != nil {
		ws.log.Errorf("Error WS json write: %v", err)
	}
	return err
}

// WriteMessage writes a message, counting towards writes before close.
func (ws *ConcurrentWebSocket) WriteMessage(messageType int, data []byte) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.closeSent || ws.closed {
		ws.log.Debugf("WS write after close: dropped")
		return ErrClosing
	}
	err := ws.conn.WriteMessage(messageType, data)
	ws.decLocked()
	return err
}

// SetWritesBeforeClose sets the number of final writes after which the stream ends; 0 or less: never.
func (ws *ConcurrentWebSocket) SetWritesBeforeClose(n int) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.writesBeforeClose = n
}

// SetCloseGrace sets how long a closing websocket waits for the UI's answer (DefaultCloseGrace).
func (ws *ConcurrentWebSocket) SetCloseGrace(d time.Duration) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.closeGrace = d
}

func (ws *ConcurrentWebSocket) decLocked() {
	if ws.writesBeforeClose <= 0 {
		return
	}
	ws.writesBeforeClose--
	if ws.writesBeforeClose == 0 {
		ws.log.Debugf("Close ws on dec()")
		ws.startCloseLocked()
	}
}

// controlWriter is the part of *websocket.Conn that writes a control frame with a deadline.
type controlWriter interface {
	WriteControl(messageType int, data []byte, deadline time.Time) error
}

// startCloseLocked sends the close frame (once) and closes the connection after the grace period, unless CloseNow
// does it first.
func (ws *ConcurrentWebSocket) startCloseLocked() {
	if ws.closeSent || ws.closed {
		return
	}
	ws.closeSent = true
	msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
	var err error
	if cc, ok := ws.conn.(controlWriter); ok {
		// bounded: a UI that stopped reading doesn't hold the lock (or shutdown)
		err = cc.WriteControl(websocket.CloseMessage, msg, time.Now().Add(ws.closeGrace))
	} else {
		err = ws.conn.WriteMessage(websocket.CloseMessage, msg)
	}
	if err != nil {
		ws.log.Debugf("ws close frame not sent: %v", err)
		ws.closeNowLocked()
		return
	}
	ws.closeTimer = time.AfterFunc(ws.closeGrace, ws.CloseNow)
}

// Close ends the stream: it sends the close frame (once) and closes the connection after the UI's answer
// (CloseNow) or the grace period. Closing again is not an error.
func (ws *ConcurrentWebSocket) Close() error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.startCloseLocked()
	return nil
}

// CloseNow closes the connection (once): the UI answered the close frame, or went away.
func (ws *ConcurrentWebSocket) CloseNow() {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.closeNowLocked()
}

func (ws *ConcurrentWebSocket) closeNowLocked() {
	if ws.closed {
		return
	}
	ws.closed = true
	if ws.closeTimer != nil {
		ws.closeTimer.Stop()
	}
	if err := ws.conn.Close(); err != nil {
		ws.log.Debugf("Close ws: %v", err)
	} else {
		ws.log.Debugf("Close ws")
	}
}

func NewWebSocketCache() WebSocketCache {
	return WebSocketCache{
		m: map[string]*ConcurrentWebSocket{},
	}
}

type WebSocketCache struct {
	m  map[string]*ConcurrentWebSocket
	mu sync.RWMutex
}

func (c *WebSocketCache) Get(key string) *ConcurrentWebSocket {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.m[key]
}

func (c *WebSocketCache) Set(key string, ws *ConcurrentWebSocket) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = ws
}

func (c *WebSocketCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}

func (c *WebSocketCache) CloseConnections() error {
	c.mu.RLock()
	conns := make([]*ConcurrentWebSocket, 0, len(c.m))
	for _, conn := range c.m {
		conns = append(conns, conn)
	}
	c.mu.RUnlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
	return nil
}
