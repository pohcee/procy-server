package outbox

import (
	"context"
	"strings"
	"time"

	"github.com/pohcee/procy-server/share/logger"
)

// Status is what the database did with a shipped item.
type Status struct {
	// Key is the item's key.
	Key string
	// Outcome is "applied", "duplicate" or "rejected: <why>".
	Outcome string
}

// Sink delivers items to the database.
type Sink interface {
	// Ship delivers items, all of one kind (at most a Shipper's Batch). It returns the outcome per item (keys of
	// items not listed stay in the outbox), or an error when the batch didn't reach the database (all stay).
	Ship(ctx context.Context, kind Kind, items []Item) ([]Status, error)
}

// Shipper ships the outbox's items through a Sink: kind by kind in the order of Kinds, oldest first, in batches,
// whenever items are added and at least every Interval; after a failure it waits, doubling the wait up to
// MaxBackoff. A kind that fails doesn't hold up the others. Shipped items (applied and duplicates) are removed;
// rejected ones are logged and removed too, except job results, which go to the dead letters (never dropped).
type Shipper struct {
	Outbox *Outbox
	Sink   Sink
	// Batch is the most items per Ship (default 500).
	Batch int
	// MaxBatchBytes caps the payload bytes per Ship (default 4 MiB); a batch has at least one item.
	MaxBatchBytes int
	// Interval is the longest wait between rounds (default 5 s).
	Interval time.Duration
	// MinBackoff and MaxBackoff bound the wait after a failure (defaults 500 ms and 30 s).
	MinBackoff, MaxBackoff time.Duration
	Log                    *logger.Logger
}

func (s *Shipper) defaults() {
	if s.Batch <= 0 {
		s.Batch = 500
	}
	if s.MaxBatchBytes <= 0 {
		s.MaxBatchBytes = 4 << 20
	}
	if s.Interval <= 0 {
		s.Interval = 5 * time.Second
	}
	if s.MinBackoff <= 0 {
		s.MinBackoff = 500 * time.Millisecond
	}
	if s.MaxBackoff <= 0 {
		s.MaxBackoff = 30 * time.Second
	}
}

// Run ships until ctx is done.
func (s *Shipper) Run(ctx context.Context) {
	s.defaults()
	backoff := time.Duration(0)
	for {
		wait := s.Interval
		if _, err := s.ShipAll(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			if backoff == 0 {
				backoff = s.MinBackoff
			} else if backoff *= 2; backoff > s.MaxBackoff {
				backoff = s.MaxBackoff
			}
			wait = backoff
			if s.Log != nil {
				s.Log.Errorf("outbox: shipping failed, retrying in %s: %v", backoff, err)
			}
		} else {
			backoff = 0
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-s.Outbox.Wake():
			t.Stop()
			if backoff > 0 { // don't hammer a database that just failed
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
			}
		}
	}
}

// ShipAll ships every pending item once and returns how many it removed. A kind that fails is left for the next
// round; the other kinds are still shipped, and the first error is returned.
func (s *Shipper) ShipAll(ctx context.Context) (int, error) {
	s.defaults()
	total := 0
	var firstErr error
	for _, kind := range Kinds {
		n, err := s.shipKind(ctx, kind)
		total += n
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return total, firstErr
}

func (s *Shipper) shipKind(ctx context.Context, kind Kind) (int, error) {
	total := 0
	for {
		items, err := s.Outbox.Pending(ctx, kind, s.Batch)
		if err != nil {
			return total, err
		}
		if len(items) == 0 {
			return total, nil
		}
		full := len(items) == s.Batch
		size := 0
		for i, it := range items {
			size += len(it.Payload)
			if i > 0 && size > s.MaxBatchBytes {
				items, full = items[:i], true
				break
			}
		}
		statuses, err := s.Sink.Ship(ctx, kind, items)
		if err != nil {
			return total, err
		}
		outcomes := outcomesOf(items, statuses)
		var ids []int64
		for i, it := range items {
			outcome, ok := outcomes[i]
			if !ok {
				continue
			}
			if strings.HasPrefix(outcome, "rejected") {
				if s.Log != nil {
					s.Log.Errorf("outbox: %s %q of tenant %s %s", kind, it.Key, it.Tenant, outcome)
				}
				if kind == KindJobResult {
					if err := s.Outbox.DeadLetter(ctx, it, outcome); err != nil {
						return total, err
					}
					total++
					continue
				}
			}
			ids = append(ids, it.ID)
		}
		if err := s.Outbox.Remove(ctx, ids); err != nil {
			return total, err
		}
		total += len(ids)
		if len(outcomes) < len(items) || !full {
			return total, nil // the rest next round (an item the sink didn't answer for stays)
		}
	}
}

// outcomesOf matches statuses to items: by position when the sink answered for every item in order (keys may
// repeat across tenants), by key otherwise.
func outcomesOf(items []Item, statuses []Status) map[int]string {
	out := map[int]string{}
	if len(statuses) == len(items) {
		inOrder := true
		for i := range items {
			if statuses[i].Key != items[i].Key {
				inOrder = false
				break
			}
		}
		if inOrder {
			for i, st := range statuses {
				out[i] = st.Outcome
			}
			return out
		}
	}
	byKey := map[string]string{}
	for _, st := range statuses {
		byKey[st.Key] = st.Outcome
	}
	for i, it := range items {
		if o, ok := byKey[it.Key]; ok {
			out[i] = o
		}
	}
	return out
}
