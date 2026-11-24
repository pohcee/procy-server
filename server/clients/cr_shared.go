package clients

import (
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/share/tenant"
)

// LoadedTenants returns the tenants whose clients the cache holds.
func (r *ClientRepository) LoadedTenants() []tenant.ID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]tenant.ID, 0, len(r.clientState))
	for tid := range r.clientState {
		out = append(out, tid)
	}
	return out
}

// SyncShared makes the cached clients of tenant tid agree with a store other server instances share: stored are
// the tenant's stored clients, connected the ids of the clients connected to some instance (the cluster's session
// registry). A client whose connection this instance holds keeps its cached object. Any other client is replaced by
// its stored record, connected when it is in connected and disconnected otherwise (since the stored time, or now
// when the record says connected), so the connection state comes from the registry rather than from whichever
// instance saved the record last. Cached clients missing from stored are dropped unless held here; new ones are
// added. A tenant that isn't loaded is left alone (its first use loads it). Nothing is saved.
func (r *ClientRepository) SyncShared(tid tenant.ID, stored []*clientdata.Client, connected map[string]bool) {
	now := clientdata.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	cached, loaded := r.clientState[tid]
	if !loaded {
		return
	}
	seen := make(map[string]bool, len(stored))
	for _, s := range stored {
		id := s.GetID()
		seen[id] = true
		if c := cached[id]; c != nil && heldHere(c) {
			continue
		}
		if connected[id] {
			s.SetDisconnectedAt(nil)
		} else if s.GetDisconnectedAt() == nil {
			at := now
			if c := cached[id]; c != nil && c.GetDisconnectedAt() != nil {
				at = *c.GetDisconnectedAt()
			}
			s.SetDisconnectedAt(&at)
		}
		cached[id] = s
	}
	for id, c := range cached {
		if !seen[id] && !heldHere(c) {
			delete(cached, id)
		}
	}
}

// heldHere reports whether this instance holds the client's connection.
func heldHere(c *clientdata.Client) bool {
	return c.GetConnection() != nil && c.IsConnected()
}
