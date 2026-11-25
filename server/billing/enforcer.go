package billing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

// PolicyRefresh is how often a process reads the transfer policies (a variable for tests).
var PolicyRefresh = time.Minute

// Fail-open timings of the processes (Limits and protections).
const (
	// PolicyKeep is how long a process keeps the last policies it read while it can't read them; then it reverts
	// to "normal" with only the plans' per-tunnel rates.
	PolicyKeep = time.Hour
	// PolicyStale: a hard cap the enforcement upkeep wrote longer ago than this is not applied (the upkeep, which
	// runs every EnforceInterval, stopped).
	PolicyStale = time.Hour
)

// ErrTransferLimit is wrapped by the error refusing a new tunnel or upload at the hard cap.
var ErrTransferLimit = errors.New("transfer limit reached")

// ErrCodeTransferLimit is the API error code (with 403) of a refusal at the hard cap.
const ErrCodeTransferLimit = "ERR_CODE_TRANSFER_LIMIT"

// TenantPolicy is an organization's transfer policy as processes read it (device_transfer_policy()).
type TenantPolicy struct {
	Organization string        `db:"tenant_id"`
	Plan         string        `db:"plan"`
	PlanName     string        `db:"-"`
	State        TransferState `db:"transfer_state"`
	TunnelRate   RateLimit     `db:"tunnel_rate"`
	ThrottleRate RateLimit     `db:"throttle_rate"`
	UpdatedAt    time.Time     `db:"updated_at"`
}

// PolicySource reads the transfer policies of every organization.
type PolicySource interface {
	TransferPolicies(ctx context.Context) ([]TenantPolicy, error)
}

// Enforcer applies the transfer policies in a process: it reads them every PolicyRefresh and answers, per tenant,
// whether a new tunnel or upload is allowed and the rate of each tunnel connection. It fails open: without a
// successful read for PolicyKeep it applies only the per-tunnel rates, and a hard cap older than PolicyStale is
// ignored. A nil *Enforcer allows everything.
type Enforcer struct {
	src   PolicySource
	plans *Plans
	log   *logger.Logger
	now   func() time.Time

	mu       sync.RWMutex
	policies map[tenant.ID]TenantPolicy
	readAt   time.Time
}

// NewEnforcer returns an Enforcer reading src; plans (optional) names the plans in errors.
func NewEnforcer(src PolicySource, plans *Plans, log *logger.Logger) *Enforcer {
	return &Enforcer{src: src, plans: plans, log: log, now: time.Now, policies: map[tenant.ID]TenantPolicy{}}
}

// Refresh reads the policies once.
func (e *Enforcer) Refresh(ctx context.Context) error {
	rows, err := e.src.TransferPolicies(ctx)
	if err != nil {
		return err
	}
	m := make(map[tenant.ID]TenantPolicy, len(rows))
	for _, r := range rows {
		if pl := e.plans.Get(r.Plan); pl != nil {
			r.PlanName = pl.Name
		}
		m[tenant.ID(r.Organization)] = r
	}
	e.mu.Lock()
	e.policies, e.readAt = m, e.now()
	e.mu.Unlock()
	return nil
}

// Run refreshes the policies every PolicyRefresh until ctx ends.
func (e *Enforcer) Run(ctx context.Context) {
	if e == nil {
		return
	}
	failing := false
	refresh := func() {
		if err := e.Refresh(ctx); err != nil {
			if ctx.Err() == nil && !failing && e.log != nil {
				e.log.Errorf("billing: read the transfer policies: %v (keeping the last ones for up to %s)", err, PolicyKeep)
			}
			failing = true
			return
		}
		if failing && e.log != nil {
			e.log.Infof("billing: the transfer policies are read again")
		}
		failing = false
	}
	refresh()
	tick := time.NewTicker(PolicyRefresh)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			refresh()
		}
	}
}

// policy returns tid's policy and whether its hard cap applies.
func (e *Enforcer) policy(tid tenant.ID) (TenantPolicy, bool, bool) {
	e.mu.RLock()
	p, ok := e.policies[tid]
	readAt := e.readAt
	e.mu.RUnlock()
	if !ok {
		return p, false, false
	}
	now := e.now()
	capped := p.State == TransferCapped && now.Sub(readAt) <= PolicyKeep && now.Sub(p.UpdatedAt) <= PolicyStale
	return p, capped, true
}

// Capped reports whether tid is at its hard cap.
func (e *Enforcer) Capped(tid tenant.ID) bool {
	if e == nil {
		return false
	}
	_, capped, _ := e.policy(tid)
	return capped
}

// Allow returns an error wrapping ErrTransferLimit when tid is at its hard cap, naming its plan: a new tunnel or
// upload is refused. Existing tunnels and device sessions are never closed.
func (e *Enforcer) Allow(tid tenant.ID) error {
	if e == nil {
		return nil
	}
	p, capped, _ := e.policy(tid)
	if !capped {
		return nil
	}
	name := p.PlanName
	if name == "" {
		name = p.Plan
	}
	return fmt.Errorf("%w: the organization has used the transfer allowance of its plan %q for this month; "+
		"new tunnels and uploads are refused and tunnels are slowed until the next month. An administrator can "+
		"choose a larger plan or raise the spending limit", ErrTransferLimit, name)
}

// TunnelRate returns the bandwidth of each tunnel connection of tid in each direction, in bytes per second (0:
// unlimited): the throttle rate at the hard cap, the plan's per-tunnel rate otherwise.
func (e *Enforcer) TunnelRate(tid tenant.ID) int64 {
	if e == nil {
		return 0
	}
	p, capped, ok := e.policy(tid)
	if !ok {
		return 0
	}
	rate := p.TunnelRate
	if capped && p.ThrottleRate > 0 && (rate == 0 || p.ThrottleRate < rate) {
		rate = p.ThrottleRate
	}
	return int64(rate)
}

// RateFunc returns a function reading TunnelRate(tid), for a tunnel's connections; nil for a nil Enforcer.
func (e *Enforcer) RateFunc(tid tenant.ID) func() int64 {
	if e == nil {
		return nil
	}
	return func() int64 { return e.TunnelRate(tid) }
}
