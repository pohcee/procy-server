package clients

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadInitialClientsMarksConnectedDisconnected(t *testing.T) {
	store := newMemStore()
	b := ctxFor(tenantB)
	store.put(t, tenantB, New(t).ID("c1").Logger(testLog).Build())

	all, err := LoadInitialClients(b, store, testLog)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.False(t, all[0].IsConnected())
	assert.Equal(t, 1, store.saveCount(), "the disconnect is saved")
}

func TestKeepConnectionStateOnLoad(t *testing.T) {
	store := newMemStore()
	b := ctxFor(tenantB)
	store.put(t, tenantB, New(t).ID("c1").Logger(testLog).Build())
	kept := KeepConnectionStateOnLoad(store)

	all, err := LoadInitialClients(b, kept, testLog)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.True(t, all[0].IsConnected(), "a client stored as connected stays so")
	assert.Zero(t, store.saveCount(), "nothing is saved")

	// a repository on it loads tenants the same way
	repo := NewClientRepositoryWithDB(nil, &hour, kept, testLog)
	assert.Len(t, repo.GetAllActiveClients(b), 1)
	assert.Zero(t, store.saveCount())

	// it forwards Get, and Refresh works through it
	getter, ok := kept.(ClientGetter)
	require.True(t, ok, "Get is forwarded when the store has it")
	got, err := getter.Get(b, "c1", testLog)
	require.NoError(t, err)
	require.NotNil(t, got)
	store.remove(tenantB, "c1")
	require.NoError(t, repo.Refresh(b, "c1"))
	assert.Empty(t, repo.GetAllClients(b))

	// and the other methods
	require.NoError(t, kept.Save(b, New(t).ID("c2").Logger(testLog).Build()))
	assert.Equal(t, 1, store.saveCount())
	require.NoError(t, kept.Delete(b, "c2", testLog))
	require.NoError(t, kept.DeleteObsolete(b, testLog))
	require.NoError(t, kept.Close())
	left, err := store.GetAll(b, testLog)
	require.NoError(t, err)
	assert.Empty(t, left)
}

func TestKeepConnectionStateOnLoadWithoutGetter(t *testing.T) {
	kept := KeepConnectionStateOnLoad(getterlessStore{newMemStore()})
	_, ok := kept.(ClientGetter)
	assert.False(t, ok, "no Get when the store has none")
}
