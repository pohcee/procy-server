package chserver

import (
	"context"
	"time"

	"github.com/openrport/openrport/server/api/jobs/schedule"
	"github.com/openrport/openrport/server/cluster"
	notificationsSQLite "github.com/openrport/openrport/server/notifications/repository/sqlite"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

// newScheduleManager returns the schedule manager. Standalone it holds the schedules of ctx's tenant, as always. In
// a cluster it holds the schedules of every active tenant, every node runs them, and each run is claimed first so it
// starts on one node only.
func newScheduleManager(ctx context.Context, c cluster.Coordinator, log *logger.Logger, p schedule.Provider, runner schedule.JobRunner, timeoutSec int) (*schedule.Manager, error) {
	if !c.Clustered() {
		return schedule.New(ctx, log, p, runner, timeoutSec)
	}
	m := schedule.NewManager(ctx, runner, p, log, timeoutSec)
	m.SetClaimer(c)
	tenants, err := c.Tenants(ctx)
	if err != nil {
		m.Stop()
		return nil, err
	}
	for _, tid := range tenants {
		if err := m.Load(tenant.WithID(ctx, tid)); err != nil {
			m.Stop()
			return nil, err
		}
	}
	return m, nil
}

// runClusterNotificationsCleanup deletes old notifications of every tenant on one node at a time. Standalone the
// API listener's cleaner does it.
func (s *Server) runClusterNotificationsCleanup(ctx context.Context) {
	if !s.cluster.Clustered() {
		return
	}
	cleanable, ok := s.repos.Notifications.(notificationsSQLite.Cleanable)
	if !ok {
		return
	}
	keep := s.config.Notifications.LogStorageDuration
	task := taskFunc(func(ctx context.Context) error {
		return cleanable.DeleteBefore(ctx, time.Now().Add(-keep).UTC())
	})
	go runPeriodic(ctx, s.cluster, s.Logger.Fork("notifications cleaner"), "notifications-cleanup", task, s.config.Notifications.CleanupInterval)
}
