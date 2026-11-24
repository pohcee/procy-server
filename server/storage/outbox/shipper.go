package outbox

import (
	"context"
	"time"

	"github.com/openrport/openrport/share/logger"
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
// MaxBackoff. Shipped items (applied, duplicates and rejected ones) are removed; rejected ones are logged.
type Shipper struct {
	Outbox *Outbox
	Sink   Sink
	// Batch is the most items per Ship (default 500).
	Batch int
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

// ShipAll ships every pending item once and returns how many it removed. It stops at the first failure.
func (s *Shipper) ShipAll(ctx context.Context) (int, error) {
	s.defaults()
	total := 0
	for _, kind := range Kinds {
		for {
			items, err := s.Outbox.Pending(ctx, kind, s.Batch)
			if err != nil {
				return total, err
			}
			if len(items) == 0 {
				break
			}
			statuses, err := s.Sink.Ship(ctx, kind, items)
			if err != nil {
				return total, err
			}
			done := map[string]string{}
			for _, st := range statuses {
				done[st.Key] = st.Outcome
			}
			var ids []int64
			for _, it := range items {
				outcome, ok := done[it.Key]
				if !ok {
					continue
				}
				ids = append(ids, it.ID)
				if s.Log != nil && len(outcome) >= 8 && outcome[:8] == "rejected" {
					s.Log.Errorf("outbox: %s %q of tenant %s %s; dropped", kind, it.Key, it.Tenant, outcome)
				}
			}
			if err := s.Outbox.Remove(ctx, ids); err != nil {
				return total, err
			}
			total += len(ids)
			if len(ids) < len(items) || len(items) < s.Batch {
				break // the rest next round (an item the sink didn't answer for stays)
			}
		}
	}
	return total, nil
}
