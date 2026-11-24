package clients

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

// memStore is an in-memory ClientStore with Get (a ClientGetter), by tenant. It keeps encoded copies, as a database
// would: what it returns never aliases what was saved. saves counts the Save calls.
type memStore struct {
	mu    sync.Mutex
	rows  map[tenant.ID]map[string]StoredClient
	saves int
}

var _ ClientGetter = (*memStore)(nil)

func newMemStore() *memStore { return &memStore{rows: map[tenant.ID]map[string]StoredClient{}} }

func (s *memStore) put(t *testing.T, tid tenant.ID, c *clientdata.Client) {
	t.Helper()
	sc, err := EncodeStoredClient(c)
	require.NoError(t, err)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows[tid] == nil {
		s.rows[tid] = map[string]StoredClient{}
	}
	s.rows[tid][sc.ID] = sc
}

func (s *memStore) remove(tid tenant.ID, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows[tid], id)
}

func (s *memStore) GetAll(ctx context.Context, l *logger.Logger) ([]*clientdata.Client, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res := make([]*clientdata.Client, 0, len(s.rows[tid]))
	for _, sc := range s.rows[tid] {
		c, err := DecodeStoredClient(sc, l)
		if err != nil {
			return nil, err
		}
		res = append(res, c)
	}
	return res, nil
}

func (s *memStore) Get(ctx context.Context, id string, l *logger.Logger) (*clientdata.Client, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.rows[tid][id]
	if !ok {
		return nil, nil
	}
	return DecodeStoredClient(sc, l)
}

func (s *memStore) Save(ctx context.Context, c *clientdata.Client) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	sc, err := EncodeStoredClient(c)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	if s.rows[tid] == nil {
		s.rows[tid] = map[string]StoredClient{}
	}
	s.rows[tid][sc.ID] = sc
	return nil
}

func (s *memStore) DeleteObsolete(context.Context, *logger.Logger) error { return nil }

func (s *memStore) Delete(ctx context.Context, id string, _ *logger.Logger) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	s.remove(tid, id)
	return nil
}

func (s *memStore) Close() error { return nil }

func (s *memStore) saveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

// getterlessStore is a ClientStore without Get.
type getterlessStore struct{ ClientStore }

// fakeConn stands for the live connection of a client connected to this node.
type fakeConn struct{ ssh.Conn }

const tenantB tenant.ID = "b"

// refreshFixture is a repository on a memStore whose tenant b is loaded, holding a disconnected client "off" and a
// client "on" connected to this node. The store holds both as the repository has them.
type refreshFixture struct {
	store *memStore
	repo  *ClientRepository
	ctx   context.Context
	off   *clientdata.Client
	on    *clientdata.Client
	conn  *fakeConn
}

func newRefreshFixture(t *testing.T) *refreshFixture {
	t.Helper()
	f := &refreshFixture{store: newMemStore(), ctx: ctxFor(tenantB), conn: &fakeConn{}}
	f.store.put(t, tenantB, New(t).ID("off").DisconnectedDuration(5*time.Minute).Logger(testLog).Build())
	f.repo = NewClientRepositoryWithDB(nil, &hour, f.store, testLog)
	off, err := f.repo.GetByID(f.ctx, "off") // loads the tenant
	require.NoError(t, err)
	require.NotNil(t, off)
	f.off = off

	f.on = New(t).ID("on").Connection(f.conn).Logger(testLog).Build()
	require.NoError(t, f.repo.Save(f.ctx, f.on))
	require.True(t, f.on.IsConnected())
	return f
}

// changedElsewhere returns the stored record of id as another node changed it: new name and admin-set fields, and
// device-reported fields the other node has stale.
func (f *refreshFixture) changedElsewhere(t *testing.T, id string) {
	t.Helper()
	c, err := f.store.Get(f.ctx, id, testLog)
	require.NoError(t, err)
	require.NotNil(t, c)
	c.Name = "renamed"
	c.AllowedUserGroups = []string{"admins-elsewhere"}
	c.Tags = []string{"tag-elsewhere"}
	c.Labels = map[string]string{"set": "elsewhere"}
	c.Version = "0.0.1-stale"
	c.IPv4 = []string{"10.0.0.99"}
	f.store.put(t, tenantB, c)
}

func TestRefreshDisconnectedClientTakesStoredRecord(t *testing.T) {
	f := newRefreshFixture(t)
	f.changedElsewhere(t, "off")
	saves := f.store.saveCount()

	require.NoError(t, f.repo.Refresh(f.ctx, "off"))

	got, err := f.repo.GetByID(f.ctx, "off")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "renamed", got.GetName())
	assert.Equal(t, []string{"admins-elsewhere"}, got.GetAllowedUserGroups())
	assert.Equal(t, []string{"tag-elsewhere"}, got.GetTags())
	assert.Equal(t, "0.0.1-stale", got.GetVersion(), "a client not connected here is what the store says")
	assert.Equal(t, saves, f.store.saveCount(), "nothing is saved back")
}

func TestRefreshConnectedHereClientMergesAdminFields(t *testing.T) {
	f := newRefreshFixture(t)
	wantVersion, wantIPv4, wantName := f.on.GetVersion(), f.on.GetIPv4(), f.on.GetName()
	f.changedElsewhere(t, "on")
	saves := f.store.saveCount()
	var posted int
	f.repo.SetPostSaveHandlerFn(func(*clientdata.Client) { posted++ })

	require.NoError(t, f.repo.Refresh(f.ctx, "on"))

	got, err := f.repo.GetByID(f.ctx, "on")
	require.NoError(t, err)
	require.Same(t, f.on, got, "the cached client holding the connection is kept")
	assert.Same(t, f.conn, got.GetConnection())
	assert.True(t, got.IsConnected())
	assert.Equal(t, []string{"admins-elsewhere"}, got.GetAllowedUserGroups())
	assert.Equal(t, wantVersion, got.GetVersion(), "device-reported fields stay as this node has them")
	assert.NotEqual(t, []string{"tag-elsewhere"}, got.GetTags(), "tags and labels are the client's (set through it)")
	assert.NotEqual(t, map[string]string{"set": "elsewhere"}, got.GetLabels())
	assert.Equal(t, wantIPv4, got.GetIPv4())
	assert.Equal(t, wantName, got.GetName())
	assert.Equal(t, saves, f.store.saveCount(), "nothing is saved back")
	assert.Zero(t, posted, "the post-save handler is not called")
}

func TestRefreshDeletedRow(t *testing.T) {
	f := newRefreshFixture(t)
	f.store.remove(tenantB, "off")
	f.store.remove(tenantB, "on")

	require.NoError(t, f.repo.Refresh(f.ctx, "off"))
	require.NoError(t, f.repo.Refresh(f.ctx, "on"))

	got, err := f.repo.GetByID(f.ctx, "off")
	require.NoError(t, err)
	assert.Nil(t, got, "a disconnected client deleted elsewhere is removed")
	got, err = f.repo.GetByID(f.ctx, "on")
	require.NoError(t, err)
	assert.Same(t, f.on, got, "a client connected here is kept")
}

func TestRefreshNewClient(t *testing.T) {
	f := newRefreshFixture(t)
	f.store.put(t, tenantB, New(t).ID("new").Logger(testLog).Build())

	require.NoError(t, f.repo.Refresh(f.ctx, "new"))

	got, err := f.repo.GetByID(f.ctx, "new")
	require.NoError(t, err)
	assert.NotNil(t, got, "a client added elsewhere is added")
	require.NoError(t, f.repo.Refresh(f.ctx, "missing"))
	got, err = f.repo.GetByID(f.ctx, "missing")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestRefreshUnloadedTenantIsNoOp(t *testing.T) {
	store := newMemStore()
	repo := NewClientRepositoryWithDB(nil, &hour, store, testLog)
	c := ctxFor("unloaded")

	require.NoError(t, repo.Refresh(c, "x"))

	repo.mu.RLock()
	_, loaded := repo.clientState["unloaded"]
	repo.mu.RUnlock()
	assert.False(t, loaded, "the tenant is not loaded by a refresh")
}

func TestRefreshWithoutGetterIsNoOp(t *testing.T) {
	store := newMemStore()
	repo := NewClientRepositoryWithDB(nil, &hour, getterlessStore{store}, testLog)
	b := ctxFor(tenantB)
	require.Empty(t, repo.GetAllClients(b)) // loads b
	store.put(t, tenantB, New(t).ID("new").Logger(testLog).Build())

	require.NoError(t, repo.Refresh(b, "new"))

	assert.Empty(t, repo.GetAllClients(b))
}

func TestRefreshRequiresTenant(t *testing.T) {
	repo := NewClientRepositoryWithDB(nil, &hour, newMemStore(), testLog)
	assert.Error(t, repo.Refresh(context.Background(), "x"))
}

func TestReload(t *testing.T) {
	f := newRefreshFixture(t)
	// another tenant, loaded, whose client is deleted meanwhile
	c := ctxFor("c")
	f.store.put(t, "c", New(t).ID("gone").DisconnectedDuration(time.Minute).Logger(testLog).Build())
	require.Len(t, f.repo.GetAllClients(c), 1)
	f.store.remove("c", "gone")

	f.changedElsewhere(t, "off")
	f.changedElsewhere(t, "on")
	f.store.put(t, tenantB, New(t).ID("new").Logger(testLog).Build()) // connected per the store: kept so
	wantVersion := f.on.GetVersion()
	saves := f.store.saveCount()

	require.NoError(t, f.repo.Reload(context.Background()))

	got, err := f.repo.GetByID(f.ctx, "off")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "renamed", got.GetName())
	got, err = f.repo.GetByID(f.ctx, "on")
	require.NoError(t, err)
	require.Same(t, f.on, got)
	assert.Equal(t, []string{"admins-elsewhere"}, got.GetAllowedUserGroups())
	assert.Equal(t, wantVersion, got.GetVersion())
	got, err = f.repo.GetByID(f.ctx, "new")
	require.NoError(t, err)
	require.NotNil(t, got, "a client added elsewhere is added")
	assert.False(t, got.IsConnected(), "this node holds the connections: a client it doesn't hold isn't connected")
	assert.Empty(t, f.repo.GetAllClients(c), "a client deleted elsewhere is dropped")
	assert.Equal(t, saves, f.store.saveCount(), "nothing is saved")

	// a client connected here is kept although its row is gone
	f.store.remove(tenantB, "on")
	f.store.remove(tenantB, "off")
	require.NoError(t, f.repo.Reload(context.Background()))
	got, err = f.repo.GetByID(f.ctx, "on")
	require.NoError(t, err)
	assert.Same(t, f.on, got)
	got, err = f.repo.GetByID(f.ctx, "off")
	require.NoError(t, err)
	assert.Nil(t, got)

	repo := NewClientRepositoryWithDB(nil, &hour, f.store, testLog)
	require.NoError(t, repo.Reload(context.Background()), "a repository with no tenant loaded")
}

// A node holding the client connections never takes "connected" from the store for a client it doesn't hold: the
// status check would ping (and close) a connection that isn't there.
func TestRefreshStoredConnectedNotHeldHereIsDisconnected(t *testing.T) {
	f := newRefreshFixture(t)
	c, err := f.store.Get(f.ctx, "off", testLog)
	require.NoError(t, err)
	c.SetDisconnectedAt(nil) // another node wrote it as connected
	f.store.put(t, tenantB, c)
	require.NoError(t, f.repo.Refresh(f.ctx, "off"))
	got, err := f.repo.GetByID(f.ctx, "off")
	require.NoError(t, err)
	assert.False(t, got.IsConnected())
	assert.NoError(t, got.Close(), "closing a client without a connection is harmless")
}

func TestRefreshOnNodeHoldingNoConnectionsKeepsStoredState(t *testing.T) {
	store := newMemStore()
	keep := time.Hour
	repo := NewClientRepositoryWithDB(nil, &keep, KeepConnectionStateOnLoad(store), testLog)
	ctx := ctxFor(tenantB)
	store.put(t, tenantB, New(t).ID("elsewhere").Logger(testLog).Build()) // connected to another node
	require.Len(t, repo.GetAllClients(ctx), 1)
	require.NoError(t, repo.Refresh(ctx, "elsewhere"))
	got, err := repo.GetByID(ctx, "elsewhere")
	require.NoError(t, err)
	assert.True(t, got.IsConnected(), "an api node shows the state the connected node stored")
}
