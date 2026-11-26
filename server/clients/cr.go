package clients

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pohcee/procy-server/server/cgroups"
	"github.com/pohcee/procy-server/server/clients/clientdata"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/tenant"
)

// ClientRepository is an in-memory cache of clients backed by a ClientStore. Its state is kept per tenant:
// every method works on the tenant carried by its context and never sees clients of another tenant.
// A tenant the cache holds no state for is loaded from the store on its first use (#34).
type ClientRepository struct {
	// in-memory state, by tenant and client id; a tenant has an entry once its stored clients are loaded
	clientState map[tenant.ID]map[string]*clientdata.Client
	// loads in progress, by tenant; the channel is closed when the load is over
	loads map[tenant.ID]chan struct{}
	// db based store
	clientStore ClientStore

	keepDisconnectedClients *time.Duration

	postSaveHandlerFn func(cl *clientdata.Client)

	logger *logger.Logger

	mu sync.RWMutex
}

type User interface {
	IsAdmin() bool
	GetGroups() []string
}

// NewClientRepository returns a new thread-safe in-memory cache to store client connections populated with given clients if any.
// keepDisconnectedClients is a duration to keep disconnected clients. If a client was disconnected longer than a given
// duration it will be treated as obsolete. The given clients belong to the default tenant.
func NewClientRepository(initClients []*clientdata.Client, keepDisconnectedClients *time.Duration, logger *logger.Logger) *ClientRepository {
	return NewClientRepositoryWithDB(initClients, keepDisconnectedClients, nil, logger)
}

// NewClientRepositoryWithDB @todo: used for test setup in two separate packages. need to review use as part of the test code refactoring.
// The given clients belong to the default tenant.
func NewClientRepositoryWithDB(initialClients []*clientdata.Client, keepDisconnectedClients *time.Duration, store ClientStore, logger *logger.Logger) *ClientRepository {
	return newClientRepository(tenant.Default, initialClients, keepDisconnectedClients, store, logger)
}

func newClientRepository(tid tenant.ID, initialClients []*clientdata.Client, keepDisconnectedClients *time.Duration, store ClientStore, logger *logger.Logger) *ClientRepository {
	clients := make(map[string]*clientdata.Client)
	for i := range initialClients {
		newClientID := initialClients[i].GetID()
		clients[newClientID] = initialClients[i]
	}

	return &ClientRepository{
		clientState:             map[tenant.ID]map[string]*clientdata.Client{tid: clients},
		loads:                   map[tenant.ID]chan struct{}{},
		clientStore:             store,
		logger:                  logger,
		keepDisconnectedClients: keepDisconnectedClients,
	}
}

// InitClientRepository returns a repository on store, populated with the stored clients of the
// tenant carried by ctx.
func InitClientRepository(
	ctx context.Context,
	store ClientStore,
	keepDisconnectedClients *time.Duration,
	logger *logger.Logger,
) (*ClientRepository, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	initialClients, err := LoadInitialClients(ctx, store, logger)
	if err != nil {
		return nil, err
	}

	return newClientRepository(tid, initialClients, keepDisconnectedClients, store, logger), nil
}

func (r *ClientRepository) SetPostSaveHandlerFn(handlerFn func(cl *clientdata.Client)) {
	r.postSaveHandlerFn = handlerFn
}

func (r *ClientRepository) GetPostSaveHandlerFn() (handlerFn func(cl *clientdata.Client)) {
	r.mu.RLock()
	handlerFn = r.postSaveHandlerFn
	r.mu.RUnlock()
	return handlerFn
}

func (r *ClientRepository) Save(ctx context.Context, cl *clientdata.Client) error {
	return r.save(ctx, cl, false)
}

func (r *ClientRepository) save(ctx context.Context, cl *clientdata.Client, admin bool) error {
	ts := time.Now()

	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}

	if err := r.loadTenant(ctx, tid); err != nil {
		return err
	}

	store := r.getStore()

	if store != nil {
		// the store write is not canceled with ctx (e.g. when a disconnect is saved during shutdown)
		err := storeSave(context.WithoutCancel(ctx), store, cl, admin)
		if err != nil {
			return fmt.Errorf("failed to save client: %w", err)
		}
	}

	r.updateClient(tid, cl)

	handlerFn := r.GetPostSaveHandlerFn()
	if handlerFn != nil {
		handlerFn(cl)
	}

	r.log().Debugf(
		"saved client: %s status=%s, within %s",
		cl.GetID(),
		FormatConnectionState(cl),
		time.Since(ts),
	)

	return nil
}

func (r *ClientRepository) Delete(ctx context.Context, client *clientdata.Client) error {
	clientID := client.GetID()

	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}

	if err := r.loadTenant(ctx, tid); err != nil {
		return err
	}

	r.log().Debugf("deleting client: %s status=%s", clientID, FormatConnectionState(client))

	store := r.getStore()

	if store != nil {
		err := store.Delete(context.WithoutCancel(ctx), clientID, client.Log())
		if err != nil {
			return fmt.Errorf("failed to delete a client: %w", err)
		}
	}

	r.removeClient(tid, clientID)
	return nil
}

func (r *ClientRepository) GetClientsByTag(ctx context.Context, tags []string, operator string, allowDisconnected bool) (matchingClients []*clientdata.Client, err error) {
	if _, err := tenantOf(ctx); err != nil {
		return nil, err
	}
	var availableClients []*clientdata.Client
	if allowDisconnected {
		availableClients = r.GetAllClients(ctx)
	} else {
		availableClients = r.GetAllActiveClients(ctx)
	}
	if strings.EqualFold(operator, "AND") {
		matchingClients = findMatchingANDClients(availableClients, tags)
	} else {
		matchingClients = findMatchingORClients(availableClients, tags)
	}

	return matchingClients, nil
}

// this fn doesn't lock the availableClients. please make sure not to use the main clients state array.
// the various GetXXXClient fns will return new client arrays. please use those fns to get a
// clients array copy for this fn to operate on.
func findMatchingANDClients(availableClients []*clientdata.Client, tags []string) (matchingClients []*clientdata.Client) {
	matchingClients = make([]*clientdata.Client, 0, 64)
	for _, cl := range availableClients {
		clientTags := cl.GetTags()

		foundAllTags := true
		for _, tag := range tags {
			foundTag := false
			for _, clTag := range clientTags {
				if tag == clTag {
					foundTag = true
					break
				}
			}
			if !foundTag {
				foundAllTags = false
				break
			}
		}
		if foundAllTags {
			matchingClients = append(matchingClients, cl)
		}

	}
	return matchingClients
}

// this fn doesn't lock the availableClients. please make sure not to use the main clients array.
// the various GetXXXClient fns will return new client arrays. please use those fns to get a
// clients array copy for this fn to operate on.
func findMatchingORClients(availableClients []*clientdata.Client, tags []string) (matchingClients []*clientdata.Client) {
	matchingClients = make([]*clientdata.Client, 0, 64)
	for _, cl := range availableClients {
		clientTags := cl.GetTags()
	nextClientForOR:
		for _, clTag := range clientTags {
			for _, tag := range tags {
				if tag == clTag {
					matchingClients = append(matchingClients, cl)
					break nextClientForOR
				}
			}
		}
	}
	return matchingClients
}

// DeleteObsolete deletes obsolete disconnected clients and returns them.
func (r *ClientRepository) DeleteObsolete(ctx context.Context) ([]*clientdata.Client, error) {
	r.log().Debugf("deleting obsolete clients")
	tid, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.loadTenant(ctx, tid); err != nil {
		return nil, err
	}
	store := r.getStore()

	if store != nil {
		err := store.DeleteObsolete(context.WithoutCancel(ctx), r.log())
		if err != nil {
			return nil, fmt.Errorf("failed to delete obsolete clients: %w", err)
		}
	}

	clientsToDelete := r.queryClients(tid, func(c *clientdata.Client) (match bool) {
		return c.Obsolete(r.GetKeepDisconnectedClients())
	})

	for _, client := range clientsToDelete {
		clientID := client.GetID()
		r.log().Debugf("deleting obsolete client: %s status=%s", clientID, FormatConnectionState(client))
		r.removeClient(tid, clientID)
	}

	return clientsToDelete, nil
}

// Count returns a number of non-obsolete active and disconnected clients.
func (r *ClientRepository) Count(ctx context.Context) int {
	availableClients := r.getNonObsoleteClients(ctx)
	return len(availableClients)
}

// CountActive returns a number of active clients.
func (r *ClientRepository) CountActive(ctx context.Context) (count int) {
	activeClients := r.GetAllActiveClients(ctx)
	return len(activeClients)
}

// CountDisconnected returns a number of disconnected clients.
func (r *ClientRepository) CountDisconnected(ctx context.Context) (int, error) {
	if _, err := tenantOf(ctx); err != nil {
		return 0, err
	}
	availableClients := r.getNonObsoleteClients(ctx)

	var n int
	// uses copy of clients array returned by getNonObsoleteClients
	for _, client := range availableClients {
		if !client.IsConnected() {
			n++
		}
	}
	return n, nil
}

// GetByID returns non-obsolete active or disconnected client by a given id.
func (r *ClientRepository) GetByID(ctx context.Context, id string) (*clientdata.Client, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	client := r.getClient(ctx, tid, id)

	if client != nil && client.Obsolete(r.GetKeepDisconnectedClients()) {
		return nil, nil
	}
	return client, nil
}

// GetActiveByID returns an active client by a given id.
func (r *ClientRepository) GetActiveByID(ctx context.Context, id string) (*clientdata.Client, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	client := r.getClient(ctx, tid, id)

	if client != nil && !client.IsConnected() {
		return nil, nil
	}
	return client, nil
}

// GetAllByClientAuthID @todo: make it consistent with others whether to return an error. In general it's just a cache, so should not return an err.
func (r *ClientRepository) GetAllByClientAuthID(ctx context.Context, clientAuthID string) (matchingClients []*clientdata.Client) {
	matchingClients = make([]*clientdata.Client, 0, DefaultInitialClientsArraySize)

	availableClients := r.GetAllClients(ctx)
	// uses copy of clients array returned by GetAllClients
	for _, c := range availableClients {
		if c.GetClientAuthID() == clientAuthID {
			matchingClients = append(matchingClients, c)
		}
	}
	return matchingClients
}

// GetAll returns all non-obsolete active and disconnected client clients.
func (r *ClientRepository) GetAllClients(ctx context.Context) []*clientdata.Client {
	availableClients := r.getNonObsoleteClients(ctx)
	return availableClients
}

// GetUserClients returns all non-obsolete active and disconnected clients that current user has access to
func (r *ClientRepository) GetUserClients(ctx context.Context, user User, groups []*cgroups.ClientGroup) []*clientdata.Client {
	return r.getNonObsoleteClientsByUser(ctx, user, groups)
}

// GetFilteredUserClients returns all non-obsolete active and disconnected clients that current user has access to, filtered by parameters
func (r *ClientRepository) GetFilteredUserClients(ctx context.Context, user User, filterOptions []query.FilterOption, groups []*cgroups.ClientGroup) (matchingClients []*clientdata.CalculatedClient, err error) {
	if _, err := tenantOf(ctx); err != nil {
		return nil, err
	}
	matchingClients = make([]*clientdata.CalculatedClient, 0, DefaultInitialClientsArraySize)

	clients := r.getNonObsoleteClientsByUser(ctx, user, groups)

	// uses copy of clients array returned by getNonObsoleteClientsByUser
	for _, client := range clients {
		calculatedClient := client.ToCalculated(groups)

		// we need to lock because MatchesFilters receives an interface and not a client,
		// therefore we lose our ability to lock.
		calculatedClient.GetLock().RLock()
		matches, err := query.MatchesFilters(calculatedClient, filterOptions)
		calculatedClient.GetLock().RUnlock()

		if err != nil {
			return matchingClients, err
		}

		if matches {
			matchingClients = append(matchingClients, calculatedClient)
		}

	}

	return matchingClients, nil
}

func (r *ClientRepository) getStore() (store ClientStore) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.clientStore
}

func (r *ClientRepository) GetKeepDisconnectedClients() (keep *time.Duration) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.keepDisconnectedClients
}

const DefaultInitialClientsArraySize = 64

// GetAllActiveClients returns a new client array that can be used without locks (assuming not shared)
func (r *ClientRepository) GetAllActiveClients(ctx context.Context) (matchingClients []*clientdata.Client) {
	matchingClients = r.queryTenantClients(ctx, func(c *clientdata.Client) (match bool) {
		return c.IsConnected()
	})
	return matchingClients
}

// getNonObsoleteClients returns a new client array that can be used without locks (assuming not shared)
func (r *ClientRepository) getNonObsoleteClients(ctx context.Context) (matchingClients []*clientdata.Client) {
	matchingClients = r.queryTenantClients(ctx, func(c *clientdata.Client) (match bool) {
		return !c.Obsolete(r.GetKeepDisconnectedClients())
	})
	return matchingClients
}

// getNonObsoleteByUser return connected clients the user has access to either by user group or by client group.
// returns a new client array that can be used without locks (assuming not shared)
func (r *ClientRepository) getNonObsoleteClientsByUser(ctx context.Context, user User, clientGroups []*cgroups.ClientGroup) (matchingClients []*clientdata.Client) {
	userGroups := user.GetGroups()

	matchingClients = r.queryTenantClients(ctx, func(c *clientdata.Client) (match bool) {
		if !c.Obsolete(r.GetKeepDisconnectedClients()) {
			if user.IsAdmin() || c.HasAccessViaUserGroups(userGroups) || c.UserGroupHasAccessViaClientGroup(userGroups, clientGroups) {
				return true
			}
		}
		return false
	})

	return matchingClients
}

// Tenants returns the tenants the cache holds clients for: those whose clients were loaded, because a client of
// theirs connected or their clients were asked for.
func (r *ClientRepository) Tenants() []tenant.ID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]tenant.ID, 0, len(r.clientState))
	for tid := range r.clientState {
		out = append(out, tid)
	}
	return out
}

func (r *ClientRepository) log() (l *logger.Logger) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.logger
}

type ClientQueryFn func(client *clientdata.Client) (match bool)

// queryTenantClients returns the clients of the tenant carried by ctx that match queryFn. Without a tenant
// it logs the error and returns no clients: callers of the methods using it get no error result.
func (r *ClientRepository) queryTenantClients(ctx context.Context, queryFn ClientQueryFn) []*clientdata.Client {
	tid, err := tenantOf(ctx)
	if err != nil {
		r.log().Errorf("%v", err)
		return make([]*clientdata.Client, 0)
	}
	if err := r.loadTenant(ctx, tid); err != nil {
		r.log().Errorf("%v", err)
		return make([]*clientdata.Client, 0)
	}
	return r.queryClients(tid, queryFn)
}

// queryClients returns a new slice of the tenant's clients that match queryFn. queryFn runs without the lock held.
// The tenant must have been loaded (loadTenant).
func (r *ClientRepository) queryClients(tid tenant.ID, queryFn ClientQueryFn) (matchingClients []*clientdata.Client) {
	r.mu.RLock()
	clients := make([]*clientdata.Client, 0, len(r.clientState[tid]))
	for _, c := range r.clientState[tid] {
		clients = append(clients, c)
	}
	r.mu.RUnlock()

	matchingClients = make([]*clientdata.Client, 0, DefaultInitialClientsArraySize)
	for _, c := range clients {
		if queryFn(c) {
			matchingClients = append(matchingClients, c)
		}
	}
	return matchingClients
}

// getClient returns the tenant's client, loading the tenant first if needed; nil when there is none or the
// load failed (logged).
func (r *ClientRepository) getClient(ctx context.Context, tid tenant.ID, clientID string) (client *clientdata.Client) {
	if err := r.loadTenant(ctx, tid); err != nil {
		r.log().Errorf("%v", err)
		return nil
	}
	r.mu.RLock()
	client = r.clientState[tid][clientID]
	r.mu.RUnlock()
	return client
}

func (r *ClientRepository) updateClient(tid tenant.ID, client *clientdata.Client) {
	clientID := client.GetID()

	r.mu.Lock()
	if r.clientState[tid] == nil {
		r.clientState[tid] = make(map[string]*clientdata.Client)
	}
	r.clientState[tid][clientID] = client
	r.mu.Unlock()
}

func (r *ClientRepository) removeClient(tid tenant.ID, clientID string) {
	r.mu.Lock()
	delete(r.clientState[tid], clientID)
	r.mu.Unlock()
}

// LoadTenant makes sure the cache holds the clients of the tenant carried by ctx (see loadTenant) and reports a
// failed load, which the other methods only log.
func (r *ClientRepository) LoadTenant(ctx context.Context) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	return r.loadTenant(ctx, tid)
}

// loadTenant makes sure the cache holds the state of the tenant, loading its stored clients on first use
// (LoadInitialClients, which marks the ones stored as connected disconnected unless the store is wrapped by
// KeepConnectionStateOnLoad). Without a store the tenant starts
// empty. The load runs without the main lock held and once per tenant at a time: concurrent first uses wait for
// it. A failed load is not cached; the next use tries again.
func (r *ClientRepository) loadTenant(ctx context.Context, tid tenant.ID) error {
	r.mu.RLock()
	_, loaded := r.clientState[tid]
	r.mu.RUnlock()
	if loaded {
		return nil
	}

	r.mu.Lock()
	if _, loaded := r.clientState[tid]; loaded {
		r.mu.Unlock()
		return nil
	}
	if done, loading := r.loads[tid]; loading {
		r.mu.Unlock()
		<-done
		r.mu.RLock()
		_, loaded := r.clientState[tid]
		r.mu.RUnlock()
		if !loaded {
			return fmt.Errorf("failed to load the clients of tenant %q", tid)
		}
		return nil
	}
	store := r.clientStore
	if store == nil {
		r.clientState[tid] = make(map[string]*clientdata.Client)
		r.mu.Unlock()
		return nil
	}
	done := make(chan struct{})
	r.loads[tid] = done
	l := r.logger
	r.mu.Unlock()

	// the load is not canceled with the request that happens to trigger it
	all, err := LoadInitialClients(context.WithoutCancel(ctx), store, l)

	r.mu.Lock()
	delete(r.loads, tid)
	if err == nil {
		clients := make(map[string]*clientdata.Client, len(all))
		for _, c := range all {
			clients[c.GetID()] = c
		}
		r.clientState[tid] = clients
	}
	r.mu.Unlock()
	close(done)

	if err != nil {
		return fmt.Errorf("failed to load the clients of tenant %q: %w", tid, err)
	}
	return nil
}
