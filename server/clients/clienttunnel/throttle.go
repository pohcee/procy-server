package clienttunnel

import (
	"context"
	"io"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateFunc returns the bandwidth a tunnel connection may use in each direction now, in bytes per second; 0 is
// unlimited. It is read again every throttleRecheck, so a new limit (the hard cap of billing) reaches open
// connections.
type RateFunc func() int64

// throttleRecheck is how often a throttle reads its RateFunc again.
const throttleRecheck = time.Second

// minBurst is the least a token bucket holds, so a buffer of a slow rate is not split into tiny writes.
const minBurst = 16 << 10

// throttle is a token bucket for one direction of a tunnel connection. A nil *throttle never waits.
type throttle struct {
	rate RateFunc

	mu      sync.Mutex
	lim     *rate.Limiter
	current int64
	checked time.Time
}

func newThrottle(f RateFunc) *throttle {
	if f == nil {
		return nil
	}
	return &throttle{rate: f}
}

// limiter returns the bucket in force, nil when unlimited.
func (t *throttle) limiter() *rate.Limiter {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.checked.IsZero() || now.Sub(t.checked) >= throttleRecheck {
		t.checked = now
		if r := t.rate(); r != t.current {
			t.current = r
			switch {
			case r <= 0:
				t.lim = nil
			case t.lim == nil:
				t.lim = rate.NewLimiter(rate.Limit(r), int(max(r, minBurst)))
			default:
				t.lim.SetLimit(rate.Limit(r))
				t.lim.SetBurst(int(max(r, minBurst)))
			}
		}
	}
	return t.lim
}

// take waits until n bytes may pass; it returns an error when ctx ends first.
func (t *throttle) take(ctx context.Context, n int) error {
	if t == nil || n <= 0 {
		return nil
	}
	lim := t.limiter()
	if lim == nil {
		return nil
	}
	for n > 0 {
		k := min(n, lim.Burst())
		if err := lim.WaitN(ctx, k); err != nil {
			return err
		}
		n -= k
	}
	return nil
}

// throttledChannel limits the bytes read from the device's channel (toward the user) and written to it (toward the
// device), each direction with its own bucket.
type throttledChannel struct {
	io.ReadWriteCloser
	ctx              context.Context
	toUser, toDevice *throttle
}

func newThrottledChannel(ctx context.Context, rwc io.ReadWriteCloser, f RateFunc) io.ReadWriteCloser {
	if f == nil {
		return rwc
	}
	return &throttledChannel{ReadWriteCloser: rwc, ctx: ctx, toUser: newThrottle(f), toDevice: newThrottle(f)}
}

func (c *throttledChannel) Read(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Read(p)
	if werr := c.toUser.take(c.ctx, n); werr != nil && err == nil {
		err = werr
	}
	return n, err
}

func (c *throttledChannel) Write(p []byte) (int, error) {
	if err := c.toDevice.take(c.ctx, len(p)); err != nil {
		return 0, err
	}
	return c.ReadWriteCloser.Write(p)
}
