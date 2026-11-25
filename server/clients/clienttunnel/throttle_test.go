package clienttunnel

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type nopRWC struct {
	io.Reader
	io.Writer
}

func (nopRWC) Close() error { return nil }

func TestThrottledChannelLimitsBothDirections(t *testing.T) {
	var rate atomic.Int64
	rate.Store(64 << 10) // 64 KiB/s
	src := bytes.NewReader(make([]byte, 96<<10))
	var dst bytes.Buffer
	ch := newThrottledChannel(context.Background(), nopRWC{src, &dst}, func() int64 { return rate.Load() })

	start := time.Now()
	n, err := io.Copy(io.Discard, ch) // toward the user: 64 KiB burst, then 32 KiB at 64 KiB/s
	require.NoError(t, err)
	assert.EqualValues(t, 96<<10, n)
	assert.GreaterOrEqual(t, time.Since(start), 400*time.Millisecond)

	start = time.Now()
	_, err = ch.Write(make([]byte, 96<<10))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, time.Since(start), 400*time.Millisecond, "toward the device, its own bucket")
	assert.Equal(t, 96<<10, dst.Len())
}

func TestThrottleUnlimitedAndCanceled(t *testing.T) {
	assert.Nil(t, newThrottle(nil))
	assert.NoError(t, (*throttle)(nil).take(context.Background(), 1<<30))
	unlimited := newThrottle(func() int64 { return 0 })
	start := time.Now()
	require.NoError(t, unlimited.take(context.Background(), 1<<30))
	assert.Less(t, time.Since(start), 100*time.Millisecond)

	slow := newThrottle(func() int64 { return 1 })
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, slow.take(ctx, minBurst)) // the burst
	cancel()
	assert.Error(t, slow.take(ctx, minBurst), "a closed tunnel stops waiting")
}

func TestThrottleFollowsANewRate(t *testing.T) {
	var rate atomic.Int64
	th := newThrottle(func() int64 { return rate.Load() })
	require.NoError(t, th.take(context.Background(), 1<<20))
	assert.Nil(t, th.limiter())
	rate.Store(32_000)
	th.checked = time.Time{} // as after throttleRecheck
	require.NotNil(t, th.limiter())
	assert.EqualValues(t, 32_000, th.limiter().Limit())
}
