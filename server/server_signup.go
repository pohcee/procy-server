package chserver

import (
	"context"
	"time"

	"github.com/openrport/openrport/server/cluster"
	"github.com/openrport/openrport/share/logger"
)

// signupUpkeepInterval is how often expired signups are closed and organizations whose deletion is due are deleted.
const signupUpkeepInterval = 5 * time.Minute

// runGlobalPeriodic runs fn every interval (and once at start) on one node of a cluster at a time: the node holding
// the lease (name, no tenant). fn's context has no tenant; it picks its own.
func runGlobalPeriodic(ctx context.Context, c cluster.Coordinator, log *logger.Logger, name string, fn func(ctx context.Context) error, interval time.Duration) {
	run := func() {
		if c.Clustered() {
			held, err := c.Lease(ctx, name, "", leaseTTL(interval))
			if err != nil {
				log.Errorf("%s: lease: %v", name, err)
				return
			}
			if !held {
				return
			}
		}
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			log.Errorf("%s: %v", name, err)
		}
	}
	run()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			run()
		}
	}
}
