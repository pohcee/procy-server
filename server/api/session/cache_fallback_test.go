package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCacheGetFallsBackToStorage(t *testing.T) {
	p, err := openTestSqliteProvider(":memory:", DataSourceOptions)
	require.NoError(t, err)
	defer p.Close()
	a := ctxFor("a")
	// the cache is built for the default tenant, so it holds none of a's sessions
	c, err := NewCache(ctxFor("default"), time.Hour, time.Hour, p, nil)
	require.NoError(t, err)

	// sessions saved to the storage only (e.g. before a restart, or by another process)
	liveID, err := p.Save(a, liveSession("alice"))
	require.NoError(t, err)
	expiredID, err := p.Save(a, expiredSession("alice"))
	require.NoError(t, err)

	found, got, err := c.Get(a, liveID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "alice", got.Username)
	assert.Equal(t, liveID, got.SessionID)
	assert.EqualValues(t, "a", got.TenantID)

	// it is cached: the user's sessions (read from the cache only) include it
	sessions, err := c.GetAllByUser(a, "alice")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, liveID, sessions[0].SessionID)

	found, _, err = c.Get(a, expiredID)
	require.NoError(t, err)
	assert.False(t, found, "an expired session is not returned")

	found, _, err = c.Get(ctxFor("b"), liveID)
	require.NoError(t, err)
	assert.False(t, found, "another tenant does not find it")

	_, _, err = c.Get(context.Background(), liveID)
	assert.Error(t, err)
}
