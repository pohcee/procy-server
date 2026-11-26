package clients

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/clients/clientdata"
)

// sharedStore records which kind of save the repository asked for.
type sharedStore struct {
	*memStore
	calls []string
}

func (s *sharedStore) Save(ctx context.Context, c *clientdata.Client) error {
	s.calls = append(s.calls, "full")
	return s.memStore.Save(ctx, c)
}

func (s *sharedStore) SaveAdminFields(ctx context.Context, c *clientdata.Client) error {
	s.calls = append(s.calls, "admin-fields")
	return s.memStore.Save(ctx, c)
}

func (s *sharedStore) SaveKeepingACL(ctx context.Context, c *clientdata.Client) error {
	s.calls = append(s.calls, "keeping-acl")
	return s.memStore.Save(ctx, c)
}

func TestSaveKindsOnTheNodeHoldingConnections(t *testing.T) {
	store := &sharedStore{memStore: newMemStore()}
	keep := time.Hour
	repo := NewClientRepositoryWithDB(nil, &keep, store, testLog)
	ctx := ctxFor(tenantB)
	c := New(t).ID("c1").Logger(testLog).Build()
	require.NoError(t, repo.Save(ctx, c))
	require.NoError(t, repo.SaveAdmin(ctx, c))
	assert.Equal(t, []string{"keeping-acl", "full"}, store.calls, "reported changes keep the stored ACL; an admin's ACL change is written")
}

func TestSaveKindsOnANodeHoldingNone(t *testing.T) {
	store := &sharedStore{memStore: newMemStore()}
	keep := time.Hour
	repo := NewClientRepositoryWithDB(nil, &keep, KeepConnectionStateOnLoad(store), testLog)
	ctx := ctxFor(tenantB)
	c := New(t).ID("c1").Logger(testLog).Build()
	require.NoError(t, repo.Save(ctx, c))
	require.NoError(t, repo.SaveAdmin(ctx, c))
	assert.Equal(t, []string{"admin-fields", "admin-fields"}, store.calls, "it never writes the connection state or reported fields")
}

func TestSaveKindsStandalone(t *testing.T) {
	store := newMemStore() // neither interface, as the SQLite store
	keep := time.Hour
	repo := NewClientRepositoryWithDB(nil, &keep, store, testLog)
	ctx := ctxFor(tenantB)
	c := New(t).ID("c1").Logger(testLog).Build()
	before := store.saveCount()
	require.NoError(t, repo.Save(ctx, c))
	require.NoError(t, repo.SaveAdmin(ctx, c))
	assert.Equal(t, before+2, store.saveCount())
}
