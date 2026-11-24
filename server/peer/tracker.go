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
	session    Session
	closeConn  func()
	registered bool
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
	} else {
		t.mu.Lock()
		h.registered = true
		t.mu.Unlock()
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

// Tick writes the heartbeat, registers the sessions not registered yet and renews the others once.
func (t *Tracker) Tick(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	if err := t.store.Heartbeat(ctx, t.node); err != nil {
		t.log.Errorf("node heartbeat: %v", err)
	}
	t.mu.Lock()
	var pending, renew []*heldConn
	for _, h := range t.held {
		if h.registered {
			renew = append(renew, h)
		} else {
			pending = append(pending, h)
		}
	}
	t.mu.Unlock()

	for _, h := range pending {
		if err := t.store.Register(ctx, t.node.NodeID, h.session, t.lease); err != nil {
			t.log.Errorf("client %s: session not registered yet: %v", h.session.ClientID, err)
			continue
		}
		t.mu.Lock()
		h.registered = true
		t.mu.Unlock()
	}
	if len(renew) == 0 {
		return
	}
	sessions := make([]Session, 0, len(renew))
	for _, h := range renew {
		sessions = append(sessions, h.session)
	}
	lost, err := t.store.Renew(ctx, t.node.NodeID, sessions, t.lease)
	if err != nil {
		t.log.Errorf("renew client sessions: %v", err)
		return
	}
	t.Lost(lost...)
}
