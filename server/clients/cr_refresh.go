package clients

import (
	"context"
	"fmt"

	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

// ClientGetter is implemented by a ClientStore that can read one client. Get returns (nil, nil) when the store has
// no client with the id. It is scoped to the tenant carried by its context.
type ClientGetter interface {
	Get(ctx context.Context, id string, l *logger.Logger) (*clientdata.Client, error)
}

// Refresh makes the cache agree with the store for the client id of the tenant carried by ctx, after another server
// instance sharing the store changed it. It does nothing when the tenant isn't loaded yet (its first use loads it
// fresh) or the store has no Get (ClientGetter). A client connected to this instance keeps its cached object, which
// holds the live connection and the device-reported data this instance is the authority for: it only takes the
// fields admins set (mergeAdminFields), and stays when its row is gone. Any other client is replaced by the stored
// record, or removed when there is none. Nothing is saved and the post-save handler is not called.
func (r *ClientRepository) Refresh(ctx context.Context, id string) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	getter, ok := r.getStore().(ClientGetter)
	if !ok || !r.waitLoaded(tid) {
		return nil
	}
	cached := r.cachedClient(tid, id)
	stored, err := getter.Get(ctx, id, r.log().Fork("client"))
	if err != nil {
		return fmt.Errorf("failed to refresh client %q: %w", id, err)
	}
	r.reconcile(tid, id, cached, stored)
	return nil
}

// Reload re-reads every loaded tenant's clients from the store, after changes made by other server instances may
// have been missed, and applies the rules of Refresh to each one: clients missing from the store are dropped unless
// connected to this instance, new ones added. Unlike a tenant's first load it marks no client disconnected. A
// tenant whose reload fails is left as it was; the first error is returned.
func (r *ClientRepository) Reload(ctx context.Context) error {
	store := r.getStore()
	if store == nil {
		return nil
	}
	r.mu.RLock()
	tids := make([]tenant.ID, 0, len(r.clientState))
	for tid := range r.clientState {
		tids = append(tids, tid)
	}
	r.mu.RUnlock()

	var firstErr error
	for _, tid := range tids {
		if err := r.reloadTenant(tenant.WithID(ctx, tid), tid, store); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *ClientRepository) reloadTenant(ctx context.Context, tid tenant.ID, store ClientStore) error {
	cached := r.queryClients(tid, func(*clientdata.Client) bool { return true })
	all, err := store.GetAll(ctx, r.log().Fork("client"))
	if err != nil {
		return fmt.Errorf("failed to reload the clients of tenant %q: %w", tid, err)
	}
	stored := make(map[string]*clientdata.Client, len(all))
	for _, c := range all {
		stored[c.GetID()] = c
	}
	for _, c := range cached {
		id := c.GetID()
		r.reconcile(tid, id, c, stored[id])
		delete(stored, id)
	}
	for id, c := range stored {
		r.reconcile(tid, id, nil, c)
	}
	return nil
}

// waitLoaded reports whether the tenant is loaded, waiting for a load in progress (which may have read the store
// before the change being refreshed). It does not start a load.
func (r *ClientRepository) waitLoaded(tid tenant.ID) bool {
	r.mu.RLock()
	_, loaded := r.clientState[tid]
	done, loading := r.loads[tid]
	r.mu.RUnlock()
	if loaded || !loading {
		return loaded
	}
	<-done
	r.mu.RLock()
	_, loaded = r.clientState[tid]
	r.mu.RUnlock()
	return loaded
}

func (r *ClientRepository) cachedClient(tid tenant.ID, id string) *clientdata.Client {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.clientState[tid][id]
}

// reconcile makes the tenant's cache entry for id, which was cached (nil: none) when stored (nil: no row) was read,
// agree with stored. An entry that changed meanwhile (e.g. the client connected here) is left alone.
func (r *ClientRepository) reconcile(tid tenant.ID, id string, cached, stored *clientdata.Client) {
	if cached != nil && connectedHere(cached) {
		if stored != nil {
			mergeAdminFields(cached, stored)
		}
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state, loaded := r.clientState[tid]
	if !loaded || state[id] != cached {
		return
	}
	if stored == nil {
		delete(state, id)
	} else {
		state[id] = stored
	}
}

// connectedHere reports whether the client is connected to this server instance: it holds the live connection.
func connectedHere(c *clientdata.Client) bool {
	return c.GetConnection() != nil && c.IsConnected()
}

// mergeAdminFields copies into the cached client the fields admins set through the API (on whichever instance
// served the request) from the stored record: the user groups allowed to access the client (SetACL), its tags and
// labels (the client attributes).
func mergeAdminFields(cached, stored *clientdata.Client) {
	stored.GetLock().RLock()
	groups := append([]string(nil), stored.AllowedUserGroups...)
	tags := append([]string(nil), stored.Tags...)
	var labels map[string]string
	if stored.Labels != nil {
		labels = make(map[string]string, len(stored.Labels))
		for k, v := range stored.Labels {
			labels[k] = v
		}
	}
	stored.GetLock().RUnlock()

	cached.GetLock().Lock()
	cached.AllowedUserGroups = groups
	cached.Tags = tags
	cached.Labels = labels
	cached.GetLock().Unlock()
}
