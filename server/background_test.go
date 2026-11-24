package chserver

import (
	"context"
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
}

func (f *fakeCluster) Clustered() bool { return true }
func (f *fakeCluster) Tenants(context.Context) ([]tenant.ID, error) {
	return f.tenants, nil
}
func (f *fakeCluster) Lease(_ context.Context, name string, tid tenant.ID, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leases = append(f.leases, name+"/"+string(tid)+"/"+ttl.String())
	return f.held[tid], nil
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
