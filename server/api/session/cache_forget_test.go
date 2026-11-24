package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A session deleted from the storage by another server instance stays in this cache until forgotten.
func TestCacheForget(t *testing.T) {
	p, err := openTestSqliteProvider(":memory:", DataSourceOptions)
	require.NoError(t, err)
	defer p.Close()
	a, b := ctxFor("a"), ctxFor("b")
	c, err := NewCache(a, time.Hour, time.Hour, p, nil)
	require.NoError(t, err)
	id, err := c.Save(a, liveSession("alice"))
	require.NoError(t, err)
	otherID, err := c.Save(a, liveSession("bob"))
	require.NoError(t, err)
	bID, err := c.Save(b, liveSession("carol"))
	require.NoError(t, err)
	require.NoError(t, p.Delete(a, id)) // directly, as another instance would

	found, _, err := c.Get(a, id)
	require.NoError(t, err)
	assert.True(t, found, "still cached")

	c.Forget("b", id) // another tenant's session with that id: nothing to forget
	found, _, err = c.Get(a, id)
	require.NoError(t, err)
	assert.True(t, found)

	c.Forget("a", id)
	found, _, err = c.Get(a, id)
	require.NoError(t, err)
	assert.False(t, found, "forgotten, and gone from the storage")
	found, _, err = c.Get(a, otherID)
	require.NoError(t, err)
	assert.True(t, found, "other sessions stay")
	found, _, err = c.Get(b, bID)
	require.NoError(t, err)
	assert.True(t, found)
}

func TestCacheForgetAll(t *testing.T) {
	p, err := openTestSqliteProvider(":memory:", DataSourceOptions)
	require.NoError(t, err)
	defer p.Close()
	a, b := ctxFor("a"), ctxFor("b")
	c, err := NewCache(a, time.Hour, time.Hour, p, nil)
	require.NoError(t, err)
	aID, err := c.Save(a, liveSession("alice"))
	require.NoError(t, err)
	bID, err := c.Save(b, liveSession("carol"))
	require.NoError(t, err)
	keptID, err := c.Save(a, liveSession("bob"))
	require.NoError(t, err)
	require.NoError(t, p.Delete(a, aID))
	require.NoError(t, p.Delete(b, bID))

	found, _, err := c.Get(a, aID)
	require.NoError(t, err)
	assert.True(t, found, "still cached")

	c.ForgetAll()
	found, _, err = c.Get(a, aID)
	require.NoError(t, err)
	assert.False(t, found)
	found, _, err = c.Get(b, bID)
	require.NoError(t, err)
	assert.False(t, found)
	found, _, err = c.Get(a, keptID)
	require.NoError(t, err)
	assert.True(t, found, "a stored session is read back from the storage")
}
