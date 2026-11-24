package changefeed

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

// Channel is the NOTIFY channel the feed trigger notifies when a transaction with feed rows commits.
const Channel = "rport_changes"

// Defaults of a Follower.
const (
	DefaultPollInterval = 3 * time.Second
	DefaultBatch        = 500
)

// Follower follows the feed from its head: it reads new changes whenever a notification arrives on Channel, and
// at least every PollInterval (a lost notification only adds delay), and hands them to OnChanges in order. When it
// falls behind retention it calls OnReset (drop everything derived from the feed) and continues from the new head.
type Follower struct {
	DB *sqlx.DB
	// OnChanges receives each batch in order. It must not block for long: the follower waits for it.
	OnChanges func(ctx context.Context, changes []Change)
	// OnReset is called when changes were missed (behind retention); whatever was cached must be reloaded.
	OnReset func(ctx context.Context)
	// OnError reports a failed read or a lost listening connection; the follower retries. Optional.
	OnError      func(err error)
	PollInterval time.Duration
	Batch        int
	// From, when set, is where the follower starts instead of the head (a reader resuming where it stopped).
	From *Cursor

	mu     sync.Mutex
	cursor Cursor
	ready  chan struct{}
	once   sync.Once
}

func (f *Follower) init() {
	f.once.Do(func() { f.ready = make(chan struct{}) })
}

// Ready is closed once the follower has taken the head: what it hands to OnChanges from then on is everything
// committed after that point.
func (f *Follower) Ready() <-chan struct{} {
	f.init()
	return f.ready
}

// Cursor returns the position of the follower.
func (f *Follower) Cursor() Cursor {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursor
}

// Run follows the feed until ctx is done. It returns ctx's error, or the error of taking the head at the start.
func (f *Follower) Run(ctx context.Context) error {
	f.init()
	poll := f.PollInterval
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	batch := f.Batch
	if batch <= 0 {
		batch = DefaultBatch
	}
	if f.From != nil {
		f.setCursor(*f.From)
	} else {
		head, err := Head(ctx, f.DB)
		if err != nil {
			return err
		}
		f.setCursor(head)
	}
	close(f.ready)

	wake := make(chan struct{}, 1)
	go f.listen(ctx, wake)

	timer := time.NewTimer(poll)
	defer timer.Stop()
	for {
		f.drain(ctx, batch)
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(poll)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		case <-timer.C:
		}
	}
}

// drain reads until no change is left to consume now.
func (f *Follower) drain(ctx context.Context, batch int) {
	for ctx.Err() == nil {
		cur := f.Cursor()
		changes, next, err := Read(ctx, f.DB, cur, batch)
		if errors.Is(err, ErrTooFarBehind) {
			head, herr := Head(ctx, f.DB)
			if herr != nil {
				f.report(herr)
				return
			}
			f.setCursor(head)
			if f.OnReset != nil {
				f.OnReset(ctx)
			}
			continue
		}
		if err != nil {
			f.report(err)
			return
		}
		if len(changes) > 0 && f.OnChanges != nil {
			f.OnChanges(ctx, changes)
		}
		f.setCursor(next)
		if len(changes) < batch {
			return
		}
	}
}

func (f *Follower) setCursor(c Cursor) {
	f.mu.Lock()
	f.cursor = c
	f.mu.Unlock()
}

func (f *Follower) report(err error) {
	if f.OnError != nil && err != nil && !errors.Is(err, context.Canceled) {
		f.OnError(err)
	}
}

// listen holds one connection in LISTEN on Channel and signals wake for each notification, reconnecting after a
// failure (polling covers the gap).
func (f *Follower) listen(ctx context.Context, wake chan<- struct{}) {
	for ctx.Err() == nil {
		err := f.listenOnce(ctx, wake)
		if ctx.Err() != nil {
			return
		}
		f.report(fmt.Errorf("changefeed: listen: %w", err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (f *Follower) listenOnce(ctx context.Context, wake chan<- struct{}) error {
	conn, err := f.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(dc any) error {
		sc, ok := dc.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("unexpected driver connection %T", dc)
		}
		pc := sc.Conn()
		// the connection never goes back to the pool listening: it is closed when this returns
		defer func() { _ = pc.Close(context.Background()) }()
		if _, err := pc.Exec(ctx, "LISTEN "+Channel); err != nil {
			return err
		}
		for {
			if _, err := pc.WaitForNotification(ctx); err != nil {
				return err
			}
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	})
}
