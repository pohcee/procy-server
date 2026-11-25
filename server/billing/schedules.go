package billing

import (
	"context"
	"time"

	"github.com/openrport/openrport/server/cluster"
	"github.com/openrport/openrport/share/tenant"
)

// PauseSchedules wraps a cluster coordinator so that no node starts a scheduled run of an organization that is
// read-only after an overdue payment (its schedules pause; they run again once it is restored). Everything else
// passes through.
func PauseSchedules(c cluster.Coordinator, svc *Service) cluster.Coordinator {
	return scheduleGate{Coordinator: c, svc: svc}
}

type scheduleGate struct {
	cluster.Coordinator
	svc *Service
}

// ClaimRun implements cluster.Coordinator.
func (g scheduleGate) ClaimRun(ctx context.Context, scheduleID string, fire time.Time) (bool, error) {
	if tid, err := tenant.FromContext(ctx); err == nil && g.svc.Access(ctx, string(tid)) == AccessReadOnly {
		g.svc.log.Infof("billing: scheduled run of %s in %s skipped: the organization is read-only (payment overdue)", scheduleID, tid)
		return false, nil
	}
	return g.Coordinator.ClaimRun(ctx, scheduleID, fire)
}
