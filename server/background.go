package chserver

import (
	"context"
	"fmt"
	"time"

	"github.com/openrport/openrport/server/cluster"
	"github.com/openrport/openrport/server/scheduler"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

// minLeaseMargin is the least a lease outlives the interval of its task, so the holder renews it before it expires.
const minLeaseMargin = 30 * time.Second

// leaseTTL is how long a node holds the lease of a task that runs every interval: until a little after its next
// run, so a node that stops hands the task over within about one and a half intervals.
func leaseTTL(interval time.Duration) time.Duration {
	margin := interval / 2
	if margin < minLeaseMargin {
		margin = minLeaseMargin
	}
	return interval + margin
}

// tenantRefreshInterval is how often clustered periodic work looks for tenants that became active (created, or active
// again) since it last listed them, so their work starts then rather than at the task's next run (an hour for some).
var tenantRefreshInterval = time.Minute

// runPeriodic runs task every interval. Standalone it runs as it always has, for the server's tenant (ctx). In a
// cluster each run goes through the active tenants and runs task for those whose lease (name, tenant) this node
// holds, so the task runs on one node at a time per tenant; the first run is at start, and a tenant that becomes
// active in between is run within tenantRefreshInterval.
func runPeriodic(ctx context.Context, c cluster.Coordinator, log *logger.Logger, name string, task scheduler.Task, interval time.Duration) {
	if !c.Clustered() {
		scheduler.Run(ctx, log, task, interval)
		return
	}
	ttl := leaseTTL(interval)
	// a first run at start: with nodes restarting more often than a long interval, the task would never run
	seen := runForTenants(ctx, c, log, name, ttl, task.Run, nil)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	refresh := time.NewTicker(tenantRefreshInterval)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if listed := runForTenants(ctx, c, log, name, ttl, task.Run, nil); listed != nil {
				seen = listed
			}
		case <-refresh.C:
			if listed := runForTenants(ctx, c, log, name, ttl, task.Run, seen); listed != nil {
				seen = listed
			}
		}
	}
}

// followNewTenants runs fn, every tenantRefreshInterval until ctx is done, for the active tenants that are not in seen
// (the tenants an earlier runForTenants listed) and whose lease (name, tenant) this node gets for ttl.
func followNewTenants(ctx context.Context, c cluster.Coordinator, log *logger.Logger, name string, ttl time.Duration, fn func(ctx context.Context) error, seen map[tenant.ID]bool) {
	refresh := time.NewTicker(tenantRefreshInterval)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh.C:
			if listed := runForTenants(ctx, c, log, name, ttl, fn, seen); listed != nil {
				seen = listed
			}
		}
	}
}

// runForTenants runs fn once for each active tenant not in skip whose lease (name, tenant) this node holds for ttl.
// It returns the active tenants it listed, or nil when listing them failed.
func runForTenants(ctx context.Context, c cluster.Coordinator, log *logger.Logger, name string, ttl time.Duration, fn func(ctx context.Context) error, skip map[tenant.ID]bool) map[tenant.ID]bool {
	tenants, err := c.Tenants(ctx)
	if err != nil {
		log.Errorf("%s: list tenants: %v", name, err)
		return nil
	}
	listed := make(map[tenant.ID]bool, len(tenants))
	for _, tid := range tenants {
		listed[tid] = true
	}
	for _, tid := range tenants {
		if ctx.Err() != nil {
			return listed
		}
		if skip[tid] {
			continue
		}
		held, err := c.Lease(ctx, name, tid, ttl)
		if err != nil {
			log.Errorf("%s: lease for tenant %s: %v", name, tid, err)
			continue
		}
		if !held {
			continue
		}
		if err := fn(tenant.WithID(ctx, tid)); err != nil {
			log.Errorf("%s: tenant %s: finished with an error: %v", name, tid, err)
		}
	}
	return listed
}

// taskFunc adapts a function to a scheduler.Task.
type taskFunc func(ctx context.Context) error

func (f taskFunc) Run(ctx context.Context) error { return f(ctx) }

func leaseName(task string, version string) string {
	if version == "" {
		return task
	}
	return fmt.Sprintf("%s/%s", task, version)
}
