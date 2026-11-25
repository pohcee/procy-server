package chserver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/openrport/openrport/server/cluster"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

type fakeCluster struct {
	cluster.Standalone
	tenants []tenant.ID
	held    map[tenant.ID]bool
	mu      sync.Mutex
	leases  []string
	// leaseErrs fail the next Lease calls of a tenant
	leaseErrs map[tenant.ID]int
}

func (f *fakeCluster) Clustered() bool { return true }
func (f *fakeCluster) Tenants(context.Context) ([]tenant.ID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tenant.ID(nil), f.tenants...), nil
}
func (f *fakeCluster) Lease(_ context.Context, name string, tid tenant.ID, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leases = append(f.leases, name+"/"+string(tid)+"/"+ttl.String())
	if f.leaseErrs[tid] > 0 {
		f.leaseErrs[tid]--
		return false, errors.New("database down")
	}
	return f.held == nil || f.held[tid], nil
}

func (f *fakeCluster) setTenants(tids ...tenant.ID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenants = tids
}

type tenantRecorder struct {
	mu   sync.Mutex
	runs []tenant.ID
}

func (r *tenantRecorder) Run(ctx context.Context) error {
	tid, _ := tenant.FromContext(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, tid)
	return nil
}

func (r *tenantRecorder) got() []tenant.ID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]tenant.ID(nil), r.runs...)
}

func TestRunPeriodicStandaloneRunsForServerTenant(t *testing.T) {
	ctx, cancel := context.WithCancel(tenant.WithID(context.Background(), tenant.Default))
	defer cancel()
	r := &tenantRecorder{}
	go runPeriodic(ctx, cluster.Standalone{}, logger.NewLogger("t", logger.LogOutput{}, logger.LogLevelError), "x", r, 20*time.Millisecond)
	assert.Eventually(t, func() bool { return len(r.got()) >= 2 }, 5*time.Second, 10*time.Millisecond)
	for _, tid := range r.got() {
		assert.Equal(t, tenant.Default, tid)
	}
}

func TestRunPeriodicClusterRunsForEachTenantItLeases(t *testing.T) {
	ctx, cancel := context.WithCancel(tenant.WithID(context.Background(), tenant.Default))
	defer cancel()
	c := &fakeCluster{tenants: []tenant.ID{"a", "b", "c"}, held: map[tenant.ID]bool{"a": true, "c": true}}
	r := &tenantRecorder{}
	go runPeriodic(ctx, c, logger.NewLogger("t", logger.LogOutput{}, logger.LogLevelError), "jobs-cleanup", r, 20*time.Millisecond)
	assert.Eventually(t, func() bool { return len(r.got()) >= 4 }, 5*time.Second, 10*time.Millisecond)
	cancel()
	for _, tid := range r.got() {
		assert.Contains(t, []tenant.ID{"a", "c"}, tid, "only the tenants whose lease this node holds")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	assert.Contains(t, c.leases, "jobs-cleanup/b/"+leaseTTL(20*time.Millisecond).String())
}

func TestLeaseTTL(t *testing.T) {
	assert.Equal(t, 90*time.Minute, leaseTTL(time.Hour))
	assert.Equal(t, 50*time.Second, leaseTTL(20*time.Second), "at least 30s past the next run")
}

// A tenant that becomes active (a new organization, or one active again) gets its periodic work within the tenant
// refresh interval, not only at the next run of a task with a long interval; the other tenants are not run again.
func TestRunPeriodicPicksUpNewTenantsPromptly(t *testing.T) {
	defer func(d time.Duration) { tenantRefreshInterval = d }(tenantRefreshInterval)
	tenantRefreshInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(tenant.WithID(context.Background(), tenant.Default))
	defer cancel()
	c := &fakeCluster{tenants: []tenant.ID{"a"}}
	r := &tenantRecorder{}
	go runPeriodic(ctx, c, logger.NewLogger("t", logger.LogOutput{}, logger.LogLevelError), "jobs-cleanup", r, time.Hour)
	assert.Eventually(t, func() bool { return len(r.got()) == 1 }, 5*time.Second, 5*time.Millisecond)

	c.setTenants("a", "b")
	assert.Eventually(t, func() bool { return len(r.got()) == 2 }, 5*time.Second, 5*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, []tenant.ID{"a", "b"}, r.got(), "a new tenant once, the others wait for the interval")

	// suspended, then active again
	c.setTenants("a")
	time.Sleep(100 * time.Millisecond)
	c.setTenants("a", "b")
	assert.Eventually(t, func() bool { return len(r.got()) == 3 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, tenant.ID("b"), r.got()[2])
}

// The built-in library entries of a tenant that becomes active after the start are synced within the refresh
// interval (under the built-ins lease of this version).
func TestFollowNewTenantsSyncsBuiltins(t *testing.T) {
	defer func(d time.Duration) { tenantRefreshInterval = d }(tenantRefreshInterval)
	tenantRefreshInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &fakeCluster{tenants: []tenant.ID{"a"}}
	r := &tenantRecorder{}
	seen := runForTenants(ctx, c, logger.NewLogger("t", logger.LogOutput{}, logger.LogLevelError), "library-builtins/1", time.Minute, r.Run, nil)
	go followNewTenants(ctx, c, logger.NewLogger("t", logger.LogOutput{}, logger.LogLevelError), "library-builtins/1", time.Minute, r.Run, seen)
	c.setTenants("a", "new")
	assert.Eventually(t, func() bool { return len(r.got()) == 2 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, []tenant.ID{"a", "new"}, r.got())
}

// A new tenant whose lease could not be checked is tried again at the next refresh, not at the task's next run.
func TestRunPeriodicRetriesTenantWhoseLeaseFailed(t *testing.T) {
	defer func(d time.Duration) { tenantRefreshInterval = d }(tenantRefreshInterval)
	tenantRefreshInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(tenant.WithID(context.Background(), tenant.Default))
	defer cancel()
	c := &fakeCluster{tenants: []tenant.ID{"a"}, leaseErrs: map[tenant.ID]int{"b": 1}}
	r := &tenantRecorder{}
	go runPeriodic(ctx, c, logger.NewLogger("t", logger.LogOutput{}, logger.LogLevelError), "jobs-cleanup", r, time.Hour)
	assert.Eventually(t, func() bool { return len(r.got()) == 1 }, 5*time.Second, 5*time.Millisecond)
	c.setTenants("a", "b")
	assert.Eventually(t, func() bool { return len(r.got()) == 2 }, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, []tenant.ID{"a", "b"}, r.got())
}
