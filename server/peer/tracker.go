package peer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/openrport/openrport/server/dispatch"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

// Default timings of a Tracker: sessions are renewed (and the node's heartbeat written) every RenewInterval, and a
// session not renewed for SessionLease is no longer located.
const (
	DefaultRenewInterval = 20 * time.Second
	DefaultSessionLease  = 60 * time.Second
)

// Tracker records the client connections of a device process in the session registry (dispatch.Sessions). It
// renews their leases and the node's heartbeat on a timer, and closes the connections another node took over. It
// learns about those by polling (each renewal returns them); Lost is the hook for a change feed to report them
// sooner.
type Tracker struct {
	store    SessionStore
	node     NodeInfo
	lease    time.Duration
	interval time.Duration
	log      *logger.Logger

	mu   sync.Mutex
	held map[string]*heldConn // by session id
}

type heldConn struct {
	session   Session
	closeConn func()
}

var _ dispatch.Sessions = (*Tracker)(nil)

// NewTracker returns a tracker for the device process of node. Zero lease or interval take the defaults.
func NewTracker(store SessionStore, node NodeInfo, lease, interval time.Duration, log *logger.Logger) *Tracker {
	if lease <= 0 {
		lease = DefaultSessionLease
	}
	if interval <= 0 {
		interval = DefaultRenewInterval
	}
	if log == nil {
		log = logger.NewLogger("sessions", logger.LogOutput{}, logger.LogLevelError)
	}
	return &Tracker{store: store, node: node, lease: lease, interval: interval, log: log, held: map[string]*heldConn{}}
}

func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

const storeTimeout = 10 * time.Second

// Connected registers a new connection; the newest session of a client wins everywhere. When the registry can't
// be reached the connection is kept and registered on a later renewal.
func (t *Tracker) Connected(ctx context.Context, clientID string, closeConn func()) (func(), error) {
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	h := &heldConn{session: Session{Tenant: tid, ClientID: clientID, SessionID: id}, closeConn: closeConn}
	t.mu.Lock()
	t.held[id] = h
	t.mu.Unlock()

	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer cancel()
	if err := t.store.Register(rctx, t.node.NodeID, h.session, t.lease); err != nil {
		t.log.Errorf("client %s: session not registered yet: %v", clientID, err)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			delete(t.held, id)
			t.mu.Unlock()
			ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
			defer cancel()
			if err := t.store.End(ectx, id); err != nil {
				t.log.Errorf("client %s: session end not recorded (its lease runs out): %v", clientID, err)
			}
		})
	}, nil
}

// Lost closes the connections of the given sessions: newer sessions of their clients replaced them.
func (t *Tracker) Lost(sessionIDs ...string) {
	for _, id := range sessionIDs {
		t.mu.Lock()
		h := t.held[id]
		delete(t.held, id)
		t.mu.Unlock()
		if h == nil {
			continue
		}
		t.log.Infof("client %s connected to another node: closing the connection here", h.session.ClientID)
		if h.closeConn != nil {
			h.closeConn()
		}
	}
}

// Run writes the heartbeat and renews the sessions every interval until ctx is done.
func (t *Tracker) Run(ctx context.Context) {
	t.Tick(ctx)
	tk := time.NewTicker(t.interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			t.Tick(ctx)
		}
	}
}

// Tick renews the sessions (recording those not recorded yet, unless a newer session of the client exists) and
// writes the heartbeat, each step with its own timeout.
func (t *Tracker) Tick(ctx context.Context) {
	t.mu.Lock()
	held := make([]*heldConn, 0, len(t.held))
	for _, h := range t.held {
		held = append(held, h)
	}
	t.mu.Unlock()

	if len(held) > 0 {
		sessions := make([]Session, 0, len(held))
		for _, h := range held {
			sessions = append(sessions, h.session)
		}
		rctx, cancel := context.WithTimeout(ctx, storeTimeout)
		// a session not recorded at connect (the database was down) is recorded by renewing: it doesn't replace a
		// newer session of the client, which register would
		lost, err := t.store.Renew(rctx, t.node.NodeID, sessions, t.lease)
		cancel()
		if err != nil {
			t.log.Errorf("renew client sessions: %v", err)
		} else {
			t.Lost(lost...)
		}
	}

	hctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	if err := t.store.Heartbeat(hctx, t.node); err != nil {
		t.log.Errorf("node heartbeat: %v", err)
	}
}
