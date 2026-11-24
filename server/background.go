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

// runPeriodic runs task every interval. Standalone it runs as it always has, for the server's tenant (ctx). In a
// cluster each run goes through the active tenants and runs task for those whose lease (name, tenant) this node
// holds, so the task runs on one node at a time per tenant.
func runPeriodic(ctx context.Context, c cluster.Coordinator, log *logger.Logger, name string, task scheduler.Task, interval time.Duration) {
	if !c.Clustered() {
		scheduler.Run(ctx, log, task, interval)
		return
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			runForTenants(ctx, c, log, name, leaseTTL(interval), task.Run)
		}
	}
}

// runForTenants runs fn once for each active tenant whose lease (name, tenant) this node holds for ttl.
func runForTenants(ctx context.Context, c cluster.Coordinator, log *logger.Logger, name string, ttl time.Duration, fn func(ctx context.Context) error) {
	tenants, err := c.Tenants(ctx)
	if err != nil {
		log.Errorf("%s: list tenants: %v", name, err)
		return
	}
	for _, tid := range tenants {
		if ctx.Err() != nil {
			return
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
