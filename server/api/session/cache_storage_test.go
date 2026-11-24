package session

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCacheSaveOfDeletedSessionIsNotCached(t *testing.T) {
	p, err := openTestSqliteProvider(":memory:", DataSourceOptions)
	require.NoError(t, err)
	defer p.Close()
	a := ctxFor("a")
	c, err := NewCache(a, time.Hour, time.Hour, p, nil)
	require.NoError(t, err)

	id, err := c.Save(a, liveSession("alice"))
	require.NoError(t, err)
	found, s, err := c.Get(a, id)
	require.NoError(t, err)
	require.True(t, found)

	// deleted from the storage (e.g. logout), then a request that was already in flight saves its access time
	require.NoError(t, p.Delete(a, id))
	_, err = c.Save(a, s)
	assert.True(t, errors.Is(err, ErrNotFound), "%v", err)

	found, _, err = c.Get(a, id)
	require.NoError(t, err)
	assert.False(t, found, "the deleted session is not brought back")
}

func TestCacheGetAllByUserIncludesStoredSessions(t *testing.T) {
	p, err := openTestSqliteProvider(":memory:", DataSourceOptions)
	require.NoError(t, err)
	defer p.Close()
	a := ctxFor("a")
	c, err := NewCache(ctxFor("default"), time.Hour, time.Hour, p, nil)
	require.NoError(t, err)

	storedOnly, err := p.Save(a, liveSession("alice"))
	require.NoError(t, err)
	cached, err := c.Save(a, liveSession("alice"))
	require.NoError(t, err)
	_, err = p.Save(a, expiredSession("alice"))
	require.NoError(t, err)
	_, err = p.Save(a, liveSession("bob"))
	require.NoError(t, err)

	sessions, err := c.GetAllByUser(a, "alice")
	require.NoError(t, err)
	var ids []int64
	for _, s := range sessions {
		ids = append(ids, s.SessionID)
		assert.EqualValues(t, "a", s.TenantID)
	}
	assert.ElementsMatch(t, []int64{storedOnly, cached}, ids)

	other, err := c.GetAllByUser(ctxFor("b"), "alice")
	require.NoError(t, err)
	assert.Empty(t, other)
}
