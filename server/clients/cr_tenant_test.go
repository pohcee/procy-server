package clients

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/clients/clienttunnel"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
)

func TestRepositoryRequiresTenant(t *testing.T) {
	keep := hour
	p := NewFakeClientProvider(t, &keep)
	defer p.Close()
	repo := NewClientRepositoryWithDB(nil, &keep, p, testLog)
	c := New(t).ID("c1").Logger(testLog).Build()
	require.NoError(t, repo.Save(defaultCtx, c))

	ctx := context.Background()
	assert.ErrorIs(t, repo.Save(ctx, c), tenant.ErrMissing)
	assert.ErrorIs(t, repo.Delete(ctx, c), tenant.ErrMissing)
	_, err := repo.DeleteObsolete(ctx)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = repo.GetByID(ctx, "c1")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = repo.GetActiveByID(ctx, "c1")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = repo.CountDisconnected(ctx)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = repo.GetClientsByTag(ctx, []string{"Linux"}, "OR", true)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = repo.GetFilteredUserClients(ctx, admin, []query.FilterOption{}, nil)
	assert.ErrorIs(t, err, tenant.ErrMissing)

	// methods without an error result see nothing
	assert.Zero(t, repo.Count(ctx))
	assert.Zero(t, repo.CountActive(ctx))
	assert.Empty(t, repo.GetAllClients(ctx))
	assert.Empty(t, repo.GetAllActiveClients(ctx))
	assert.Empty(t, repo.GetAllByClientAuthID(ctx, c.GetClientAuthID()))
	assert.Empty(t, repo.GetUserClients(ctx, admin, nil))

	// the client is still there for its tenant
	got, err := repo.GetByID(defaultCtx, "c1")
	require.NoError(t, err)
	assert.Equal(t, c, got)
}

func TestRepositoryIsolatesTenants(t *testing.T) {
	keep := hour
	p := NewFakeClientProvider(t, &keep)
	defer p.Close()
	repo := NewClientRepositoryWithDB(nil, &keep, p, testLog)
	a, b := ctxFor("a"), ctxFor("b")

	ca := New(t).ID("same").ClientAuthID("auth").Logger(testLog).Build()
	cb := New(t).ID("same").ClientAuthID("auth").Logger(testLog).Build()
	require.NoError(t, repo.Save(a, ca))
	require.NoError(t, repo.Save(b, cb))

	got, err := repo.GetByID(a, "same")
	require.NoError(t, err)
	assert.Same(t, ca, got)
	got, err = repo.GetActiveByID(b, "same")
	require.NoError(t, err)
	assert.Same(t, cb, got)
	got, err = repo.GetByID(ctxFor("c"), "same")
	require.NoError(t, err)
	assert.Nil(t, got)

	assert.Equal(t, 1, repo.Count(a))
	assert.Equal(t, 1, repo.CountActive(b))
	assert.Len(t, repo.GetAllClients(a), 1)
	assert.Len(t, repo.GetAllActiveClients(a), 1)
	assert.Len(t, repo.GetAllByClientAuthID(a, "auth"), 1)
	assert.Len(t, repo.GetUserClients(a, admin, nil), 1)
	filtered, err := repo.GetFilteredUserClients(a, admin, []query.FilterOption{}, nil)
	require.NoError(t, err)
	assert.Len(t, filtered, 1)
	byTag, err := repo.GetClientsByTag(a, []string{"Linux"}, "OR", true)
	require.NoError(t, err)
	assert.Len(t, byTag, 1)

	// obsolete clients are deleted only in the given tenant
	oa := New(t).ID("old").DisconnectedDuration(keep + time.Minute).Logger(testLog).Build()
	ob := New(t).ID("old").DisconnectedDuration(keep + time.Minute).Logger(testLog).Build()
	require.NoError(t, repo.Save(a, oa))
	require.NoError(t, repo.Save(b, ob))
	deleted, err := repo.DeleteObsolete(a)
	require.NoError(t, err)
	require.Len(t, deleted, 1)
	assert.Same(t, oa, deleted[0])
	stored, err := p.get(b, "old", testLog)
	require.NoError(t, err)
	assert.NotNil(t, stored)

	require.NoError(t, repo.Delete(a, ca))
	got, err = repo.GetByID(a, "same")
	require.NoError(t, err)
	assert.Nil(t, got)
	got, err = repo.GetByID(b, "same")
	require.NoError(t, err)
	assert.Same(t, cb, got)
	stored, err = p.get(b, "same", testLog)
	require.NoError(t, err)
	assert.NotNil(t, stored)
}

func TestInitClientRepositoryLoadsContextTenant(t *testing.T) {
	p := NewFakeClientProvider(t, nil)
	defer p.Close()
	require.NoError(t, p.Save(ctxFor("a"), New(t).ID("in-a").DisconnectedDuration(time.Minute).Logger(testLog).Build()))
	require.NoError(t, p.Save(ctxFor("b"), New(t).ID("in-b").DisconnectedDuration(time.Minute).Logger(testLog).Build()))

	repo, err := InitClientRepository(ctxFor("a"), p, nil, testLog)
	require.NoError(t, err)

	got, err := repo.GetByID(ctxFor("a"), "in-a")
	require.NoError(t, err)
	assert.NotNil(t, got)
	// another tenant is loaded on its first use, and sees only its own clients
	inB := repo.GetAllClients(ctxFor("b"))
	require.Len(t, inB, 1)
	assert.Equal(t, "in-b", inB[0].GetID())

	_, err = InitClientRepository(context.Background(), p, nil, testLog)
	assert.ErrorIs(t, err, tenant.ErrMissing)
}

func TestCleanupTaskUsesItsContextTenant(t *testing.T) {
	keep := hour
	p := NewFakeClientProvider(t, &keep)
	defer p.Close()
	repo := NewClientRepositoryWithDB(nil, &keep, p, testLog)
	old := New(t).ID("old").DisconnectedDuration(keep + time.Minute).Logger(testLog).Build()
	require.NoError(t, repo.Save(ctxFor("a"), old))

	task := NewCleanupTask(testLog, repo)
	assert.ErrorIs(t, task.Run(context.Background()), tenant.ErrMissing)
	require.NoError(t, task.Run(ctxFor("a")))

	stored, err := p.get(ctxFor("a"), "old", testLog)
	require.NoError(t, err)
	assert.Nil(t, stored)
}

func TestTunnelAutoCloseCleanupSavesInClientTenant(t *testing.T) {
	p := NewFakeClientProvider(t, nil)
	defer p.Close()
	repo := NewClientRepositoryWithDB(nil, nil, p, testLog)
	s := NewClientService(nil, nil, repo, testLog, nil)

	a := ctxFor("a")
	c := New(t).ID("c1").Logger(testLog).Build()
	c.Context = a
	tunnel := &clienttunnel.Tunnel{ID: "t1"}
	c.SetTunnels([]*clienttunnel.Tunnel{tunnel})
	require.NoError(t, repo.Save(a, c))

	// runs from a tunnel goroutine with no request context: the client's context carries the tenant
	s.cleanupAfterAutoClose(c, tunnel)

	stored, err := p.get(a, "c1", testLog)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Empty(t, stored.Tunnels)
}
