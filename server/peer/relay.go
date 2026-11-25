package peer

import (
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/ws"
)

// relay carries what the client listener of a device process writes to the UI job stream of a job (live output and
// the final result) back to the api process that started the job. For each running job it puts a websocket into the
// UI job stream cache, under the key the client listener looks up (the multi-client job id, or the job id), whose
// writes go to the run-job stream of that job, picked by the "jid" of each message.
type relay struct {
	streams *ws.WebSocketCache
	log     *logger.Logger

	mu      sync.Mutex
	entries map[string]*relayEntry // by stream key
}

type relayEntry struct {
	socket *ws.ConcurrentWebSocket
	sinks  map[string]*relaySink // by job id
}

type relayMsg struct {
	final bool
	data  []byte
}

type relaySink struct {
	ch chan relayMsg
}

// relayWait is how long a message waits for the run-job stream to take it.
const relayWait = 10 * time.Second

func newRelay(streams *ws.WebSocketCache, log *logger.Logger) *relay {
	return &relay{streams: streams, log: log, entries: map[string]*relayEntry{}}
}

// open starts relaying the messages of job jid, written under key; close stops it.
func (r *relay) open(key, jid string) (sink *relaySink, closeFn func()) {
	sink = &relaySink{ch: make(chan relayMsg, 256)}
	r.mu.Lock()
	e := r.entries[key]
	if e == nil {
		e = &relayEntry{sinks: map[string]*relaySink{}}
		e.socket = ws.NewConcurrentWebSocket(&relayConn{relay: r, key: key}, r.log)
		// the relay carries every job of the key (a multi-client run's jobs on this node): it never closes itself
		e.socket.SetWritesBeforeClose(0)
		r.entries[key] = e
		r.streams.Set(key, e.socket)
	}
	e.sinks[jid] = sink
	r.mu.Unlock()
	return sink, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if e.sinks[jid] == sink {
			delete(e.sinks, jid)
		}
		if len(e.sinks) == 0 && r.entries[key] == e {
			delete(r.entries, key)
			if r.streams.Get(key) == e.socket {
				r.streams.Delete(key)
			}
		}
	}
}

func (r *relay) deliver(key string, msg relayMsg) {
	var head struct {
		JID string `json:"jid"`
	}
	if err := json.Unmarshal(msg.data, &head); err != nil {
		r.log.Debugf("relay %s: message without a job id: %v", key, err)
		return
	}
	r.mu.Lock()
	var sink *relaySink
	if e := r.entries[key]; e != nil {
		sink = e.sinks[head.JID]
	}
	r.mu.Unlock()
	if sink == nil {
		return
	}
	// output and the result wait for a slow reader (the client's output channel then waits too); only an api process
	// that stopped reading loses them
	select {
	case sink.ch <- msg:
	case <-time.After(relayWait):
		r.log.Errorf("relay %s: output or result of job %s not relayed: the api process does not read", key, head.JID)
	}
}

// relayConn is the ws.Conn under a relay websocket: the client listener writes output with WriteJSON (through
// ConcurrentWebSocket.WriteNonFinalJSON) and the final result with WriteMessage.
type relayConn struct {
	relay *relay
	key   string
}

var errRelayRead = errors.New("relay websockets are write-only")

func (c *relayConn) NextReader() (int, io.Reader, error) { return 0, nil, errRelayRead }
func (c *relayConn) ReadMessage() (int, []byte, error)   { return 0, nil, errRelayRead }

func (c *relayConn) WriteMessage(messageType int, data []byte) error {
	if messageType != websocket.TextMessage {
		return nil // a close frame (the cache closing at shutdown): nothing to relay
	}
	c.relay.deliver(c.key, relayMsg{final: true, data: append([]byte(nil), data...)})
	return nil
}

func (c *relayConn) WriteJSON(v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.relay.deliver(c.key, relayMsg{data: b})
	return nil
}

// Close does nothing: the relay ends with the jobs it relays.
func (c *relayConn) Close() error { return nil }
