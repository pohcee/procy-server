package clients

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/share/logger"
)

// flakyStore wraps a ClientStore, counts the GetAll calls and fails them while failing is set.
type flakyStore struct {
	ClientStore
	failing atomic.Bool
	getAlls atomic.Int32
}

func (s *flakyStore) GetAll(ctx context.Context, l *logger.Logger) ([]*clientdata.Client, error) {
	s.getAlls.Add(1)
	if s.failing.Load() {
		return nil, errors.New("store unavailable")
	}
	return s.ClientStore.GetAll(ctx, l)
}

func TestClientRepositoryLoadsTenantOnFirstUse(t *testing.T) {
	keep := hour
	p := NewFakeClientProvider(t, &keep)
	defer p.Close()
	b := ctxFor("b")
	stored := New(t).ID("c1").Logger(testLog).Build() // connected
	require.True(t, stored.IsConnected())
	require.NoError(t, p.Save(b, stored))

	store := &flakyStore{ClientStore: p}
	repo := NewClientRepositoryWithDB(nil, &keep, store, testLog)

	got, err := repo.GetByID(b, "c1")
	require.NoError(t, err)
	require.NotNil(t, got, "the tenant's stored client is loaded on first use")
	assert.False(t, got.IsConnected(), "a client stored as connected comes back disconnected")
	assert.Len(t, repo.GetAllClients(b), 1)
	assert.Empty(t, repo.GetAllActiveClients(b))
	assert.Empty(t, repo.GetAllClients(defaultCtx), "other tenants do not see it")

	inStore, err := p.GetAll(b, testLog)
	require.NoError(t, err)
	require.Len(t, inStore, 1)
	assert.False(t, inStore[0].IsConnected(), "the disconnect is saved")

	assert.Equal(t, int32(1), store.getAlls.Load(), "the tenant is loaded once")
}

func TestClientRepositoryLazyLoadCoversEveryAccessor(t *testing.T) {
	keep := hour
	for name, use := range map[string]func(r *ClientRepository, ctx context.Context) error{
		"GetActiveByID": func(r *ClientRepository, ctx context.Context) error { _, err := r.GetActiveByID(ctx, "x"); return err },
		"Count":         func(r *ClientRepository, ctx context.Context) error { r.Count(ctx); return nil },
		"Save": func(r *ClientRepository, ctx context.Context) error {
			return r.Save(ctx, New(t).ID("other").Logger(testLog).Build())
		},
		"Delete": func(r *ClientRepository, ctx context.Context) error {
			return r.Delete(ctx, New(t).ID("other").Logger(testLog).Build())
		},
		"DeleteObsolete": func(r *ClientRepository, ctx context.Context) error { _, err := r.DeleteObsolete(ctx); return err },
	} {
		t.Run(name, func(t *testing.T) {
			p := NewFakeClientProvider(t, &keep)
			defer p.Close()
			b := ctxFor("b")
			require.NoError(t, p.Save(b, New(t).ID("c1").Logger(testLog).Build()))
			repo := NewClientRepositoryWithDB(nil, &keep, p, testLog)

			require.NoError(t, use(repo, b))

			got, err := repo.GetByID(b, "c1")
			require.NoError(t, err)
			assert.NotNil(t, got, "the stored client is still in the cache after %s", name)
		})
	}
}

func TestClientRepositoryLoadFailureNotCached(t *testing.T) {
	keep := hour
	p := NewFakeClientProvider(t, &keep)
	defer p.Close()
	b := ctxFor("b")
	require.NoError(t, p.Save(b, New(t).ID("c1").Logger(testLog).Build()))

	store := &flakyStore{ClientStore: p}
	store.failing.Store(true)
	repo := NewClientRepositoryWithDB(nil, &keep, store, testLog)

	got, err := repo.GetByID(b, "c1")
	require.NoError(t, err)
	assert.Nil(t, got, "nothing is found while the load fails")
	assert.Empty(t, repo.GetAllClients(b))
	assert.Error(t, repo.Save(b, New(t).ID("c2").Logger(testLog).Build()), "a write needs the tenant loaded")

	store.failing.Store(false)
	got, err = repo.GetByID(b, "c1")
	require.NoError(t, err)
	assert.NotNil(t, got, "the load is retried")
}

func TestClientRepositoryWithoutStoreStartsTenantsEmpty(t *testing.T) {
	repo := NewClientRepository(nil, &hour, testLog)
	b := ctxFor("b")
	assert.Empty(t, repo.GetAllClients(b))
	c := New(t).ID("c1").Logger(testLog).Build()
	require.NoError(t, repo.Save(b, c))
	got, err := repo.GetByID(b, "c1")
	require.NoError(t, err)
	assert.Same(t, c, got)
}

// Concurrent first uses load the tenant once, and saves made meanwhile are kept. Run with -race.
func TestClientRepositoryLazyLoadConcurrent(t *testing.T) {
	keep := hour
	p := NewFakeClientProvider(t, &keep)
	defer p.Close()
	b := ctxFor("b")
	require.NoError(t, p.Save(b, New(t).ID("stored").Logger(testLog).Build()))
	store := &flakyStore{ClientStore: p}
	repo := NewClientRepositoryWithDB(nil, &keep, store, testLog)

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = repo.GetAllClients(b)
		}()
		go func() {
			defer wg.Done()
			assert.NoError(t, repo.Save(b, New(t).ID(fmt.Sprintf("new-%d", i)).Logger(testLog).Build()))
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), store.getAlls.Load())
	assert.Equal(t, n+1, len(repo.GetAllClients(b)))
}
