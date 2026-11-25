package pgserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/cluster"
	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/changefeed"
	pgusage "github.com/openrport/openrport/server/storage/postgres/usage"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

// Retention of the cluster's bookkeeping.
const (
	// FeedRetention is how long change feed rows are kept; a node further behind reloads what it caches.
	FeedRetention = 3 * 24 * time.Hour
	// ScheduleRunRetention is how long schedule run claims are kept.
	ScheduleRunRetention = 7 * 24 * time.Hour
	// upkeepInterval is how often the feed and the run claims are pruned (by one node, under a lease).
	upkeepInterval = time.Hour

	// ClientListenerLease is the lease of the one node of a cluster that accepts client connections (phase 2).
	ClientListenerLease = "client-listener"
)

// clientListenerTTL and clientListenerRenew: the holder renews often, so a replacement listener node can take over
// within a minute of the old one stopping. Variables for tests.
var (
	clientListenerTTL   = time.Minute
	clientListenerRenew = 20 * time.Second
)

// Coordinator is the cluster.Coordinator of a node on Postgres: leases in work_leases, run claims in
// schedule_runs, the active tenants from tenants, and the change feed delivered to the watchers.
type Coordinator struct {
	db     *sqlx.DB
	nodeID string
	// holder names this process in leases and claims: the node id and a random suffix, so two processes started
	// with the same node id never share a lease.
	holder string
	log    *logger.Logger

	follower *changefeed.Follower

	// noRuns: this node can't do work on clients (role api, phase 2), so it leaves scheduled runs to the node that
	// can: a run it claimed would only fail as "not connected"
	noRuns bool

	// usage is how the upkeep keeps the usage tables (rolled up and pruned on every node, metering on or not)
	usage pgusage.Retention

	mu       sync.Mutex
	watchers []cluster.Watcher
	// stopRenewing stops renewing ClientListenerLease (nil: not held)
	stopRenewing context.CancelFunc
}

var _ cluster.Coordinator = (*Coordinator)(nil)

// NewCoordinator returns the coordinator of node nodeID on db. It takes the change feed's head now: the watchers
// see every change committed after this call, so call it before the server loads what it caches.
func NewCoordinator(ctx context.Context, db *sqlx.DB, nodeID string, log *logger.Logger) (*Coordinator, error) {
	if nodeID == "" {
		return nil, errors.New("cluster: node id is empty")
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return nil, err
	}
	if err := changefeed.CheckNotAhead(ctx, db); err != nil {
		return nil, err
	}
	head, err := changefeed.Head(ctx, db)
	if err != nil {
		return nil, err
	}
	c := &Coordinator{db: db, nodeID: nodeID, holder: nodeID + "/" + hex.EncodeToString(suffix), log: log}
	c.follower = &changefeed.Follower{
		DB:        db,
		From:      &head,
		OnChanges: c.deliver,
		OnReset:   c.reset,
		OnError:   func(err error) { c.log.Errorf("change feed: %v", err) },
	}
	return c, nil
}

// NodeID returns the node id of the coordinator.
func (c *Coordinator) NodeID() string { return c.nodeID }

// Cursor returns how far this node has consumed the change feed.
func (c *Coordinator) Cursor() changefeed.Cursor { return c.follower.Cursor() }

// Holder returns the name of this process in leases and claims.
func (c *Coordinator) Holder() string { return c.holder }

// Clustered is true: other nodes may share the database.
func (c *Coordinator) Clustered() bool { return true }

// Tenants lists the active tenants.
func (c *Coordinator) Tenants(ctx context.Context) ([]tenant.ID, error) {
	var ids []string
	if err := c.db.SelectContext(ctx, &ids, "SELECT id FROM tenants WHERE status = 'active' ORDER BY id"); err != nil {
		return nil, fmt.Errorf("cluster: list tenants: %w", err)
	}
	out := make([]tenant.ID, 0, len(ids))
	for _, id := range ids {
		out = append(out, tenant.ID(id))
	}
	return out, nil
}

// Lease takes, renews or fails to get the lease name of tid for this process.
func (c *Coordinator) Lease(ctx context.Context, name string, tid tenant.ID, ttl time.Duration) (bool, error) {
	return c.lease(ctx, name, tid, c.holder, ttl)
}

// lease takes the lease (name, tid) for holder until ttl from now, if it is free, expired or holder's already.
func (c *Coordinator) lease(ctx context.Context, name string, tid tenant.ID, holder string, ttl time.Duration) (bool, error) {
	var got []string
	err := c.db.SelectContext(ctx, &got, `INSERT INTO work_leases (name, tenant_id, holder, until)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4))
		ON CONFLICT (name, tenant_id) DO UPDATE SET holder = EXCLUDED.holder, until = EXCLUDED.until
			WHERE work_leases.holder = EXCLUDED.holder OR work_leases.until < now()
		RETURNING holder`, name, string(tid), holder, ttl.Seconds())
	if err != nil {
		return false, fmt.Errorf("cluster: lease %s: %w", name, err)
	}
	return len(got) == 1, nil
}

// LeaseHolder returns the holder of the lease (name, tid) while it is valid, or "".
func (c *Coordinator) LeaseHolder(ctx context.Context, name string, tid tenant.ID) (string, error) {
	var got []string
	err := c.db.SelectContext(ctx, &got, "SELECT holder FROM work_leases WHERE name = $1 AND tenant_id = $2 AND until >= now()", name, string(tid))
	if err != nil || len(got) == 0 {
		return "", err
	}
	return got[0], nil
}

// ClaimRun records the run of schedule scheduleID (tenant from ctx) due at fire, and reports whether this call
// recorded it.
func (c *Coordinator) ClaimRun(ctx context.Context, scheduleID string, fire time.Time) (bool, error) {
	if c.noRuns {
		return false, nil
	}
	var n int64
	err := postgres.InTenant(ctx, c.db, func(tx *sqlx.Tx) error {
		tid, _ := tenant.FromContext(ctx)
		res, err := tx.ExecContext(ctx, `INSERT INTO schedule_runs (tenant_id, schedule_id, fire_time, holder)
			VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, string(tid), scheduleID, fire.UTC(), c.holder)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return false, fmt.Errorf("cluster: claim run of schedule %s: %w", scheduleID, err)
	}
	return n == 1, nil
}

// KeepUsage sets how the cluster upkeep keeps the usage tables (retention, monitoring gauge); without it the
// defaults, no monitoring gauge.
func (c *Coordinator) KeepUsage(r pgusage.Retention) { c.usage = r }

// LeaveScheduledRuns makes this node claim no scheduled run (ClaimRun is always false): it can't reach the clients.
func (c *Coordinator) LeaveScheduledRuns() { c.noRuns = true }

// AcquireClientListener takes the lease of the cluster's client listener for this node, or returns an error naming
// the node holding it. The holder is the node id, so the same node restarting takes it back at once. It is renewed
// from now on, until Run ends or Release is called.
func (c *Coordinator) AcquireClientListener(ctx context.Context) error {
	ok, err := c.lease(ctx, ClientListenerLease, "", c.nodeID, clientListenerTTL)
	if err != nil {
		return err
	}
	if !ok {
		holder, _ := c.LeaseHolder(ctx, ClientListenerLease, "")
		return fmt.Errorf(`cluster: node %q accepts the client connections of this database; only one node may run with [storage] role = "all" (run the others with role = "api"), or wait a minute after that node stopped`, holder)
	}
	rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.mu.Lock()
	c.stopRenewing = cancel
	c.mu.Unlock()
	go c.renewClientListener(rctx)
	return nil
}

// Release gives up this process's leases (another node takes its work at once) and stops renewing them. Run calls
// it when it ends.
func (c *Coordinator) Release() {
	c.mu.Lock()
	stop := c.stopRenewing
	c.stopRenewing = nil
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	q := "DELETE FROM work_leases WHERE holder = $1"
	args := []any{c.holder}
	if stop != nil {
		q += " OR (name = $2 AND tenant_id = '' AND holder = $3)"
		args = append(args, ClientListenerLease, c.nodeID)
	}
	if _, err := c.db.ExecContext(ctx, q, args...); err != nil {
		c.log.Errorf("cluster: release leases: %v", err)
	}
}

// Watch adds w to the watchers.
func (c *Coordinator) Watch(w cluster.Watcher) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.watchers = append(c.watchers, w)
}

func (c *Coordinator) currentWatchers() []cluster.Watcher {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]cluster.Watcher(nil), c.watchers...)
}

func (c *Coordinator) deliver(ctx context.Context, changes []changefeed.Change) {
	out := make([]cluster.Change, 0, len(changes))
	for _, ch := range changes {
		out = append(out, cluster.Change{Tenant: ch.Tenant, Entity: ch.Entity, Key: ch.Key, Op: ch.Op})
	}
	for _, w := range c.currentWatchers() {
		w.OnChanges(ctx, out)
	}
}

func (c *Coordinator) reset(ctx context.Context) {
	c.log.Infof("change feed: this node fell behind the feed's retention; reloading what it caches")
	for _, w := range c.currentWatchers() {
		w.OnReset(ctx)
	}
}

// Run follows the change feed and prunes the feed and run claims until ctx is done; then it releases this node's
// leases.
func (c *Coordinator) Run(ctx context.Context) error {
	defer c.Release()
	go c.upkeep(ctx)
	err := c.follower.Run(ctx)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (c *Coordinator) renewClientListener(ctx context.Context) {
	tick := time.NewTicker(clientListenerRenew)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			ok, err := c.lease(ctx, ClientListenerLease, "", c.nodeID, clientListenerTTL)
			if err != nil {
				c.log.Errorf("cluster: renew the client listener lease: %v", err)
			} else if !ok {
				c.log.Errorf("cluster: another node took the client listener lease; this node keeps its connected clients until it is stopped")
			}
		}
	}
}

func (c *Coordinator) upkeep(ctx context.Context) {
	c.prune(ctx)
	tick := time.NewTicker(upkeepInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			c.prune(ctx)
		}
	}
}

// prune removes old feed rows and run claims, on one node at a time.
// IngestKeyRetention is how long the idempotency keys of shipped items are kept: far longer than a device process
// keeps an item in its outbox, or a batch shipped again after that would be applied twice.
const IngestKeyRetention = 7 * 24 * time.Hour

// UsageKeyRetention is how long the keys of usage items are kept: longer than ingest_usage accepts an hour (7 days),
// so an item shipped again after its key is gone is rejected as too old instead of added twice.
const UsageKeyRetention = 8 * 24 * time.Hour

// ConnectionEventRetention is how long client connection events are kept.
const ConnectionEventRetention = 30 * 24 * time.Hour

func (c *Coordinator) prune(ctx context.Context) {
	ok, err := c.Lease(ctx, "cluster-upkeep", "", upkeepInterval+upkeepInterval/2)
	if err != nil {
		c.log.Errorf("cluster: %v", err)
		return
	}
	if !ok {
		return
	}
	if n, err := changefeed.Prune(ctx, c.db, FeedRetention); err != nil {
		c.log.Errorf("cluster: %v", err)
	} else if n > 0 {
		c.log.Debugf("cluster: pruned %d change feed rows", n)
	}
	tenants, err := c.Tenants(ctx)
	if err != nil {
		c.log.Errorf("cluster: %v", err)
		return
	}
	for _, tid := range tenants {
		err := postgres.InTenantID(ctx, c.db, tid, func(tx *sqlx.Tx) error {
			if _, err := tx.ExecContext(ctx, "DELETE FROM schedule_runs WHERE tenant_id = $1 AND claimed_at < now() - make_interval(secs => $2)",
				string(tid), ScheduleRunRetention.Seconds()); err != nil {
				return err
			}
			// what device processes shipped (migration 1100): idempotency keys and connection events
			if _, err := tx.ExecContext(ctx, `DELETE FROM ingest_keys WHERE tenant_id = $1 AND received_at < now() - make_interval(secs => $2)
				AND (kind <> 'usage' OR received_at < now() - make_interval(secs => $3))`,
				string(tid), IngestKeyRetention.Seconds(), UsageKeyRetention.Seconds()); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, "DELETE FROM client_connection_events WHERE tenant_id = $1 AND at < now() - make_interval(secs => $2)",
				string(tid), ConnectionEventRetention.Seconds())
			return err
		})
		if err != nil {
			c.log.Errorf("cluster: prune schedule runs of tenant %s: %v", tid, err)
		}
	}
	if err := pgusage.Upkeep(ctx, c.db, c.usage); err != nil {
		c.log.Errorf("cluster: %v", err)
	}
}
