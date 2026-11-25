package chserver

import (
	"context"
	"time"

	"github.com/openrport/openrport/server/billing"
	"github.com/openrport/openrport/server/notifications"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/refs"
	"github.com/openrport/openrport/share/tenant"
)

// BillingOpts turn billing plans and limits on (Postgres nodes with [billing] enabled).
type BillingOpts struct {
	Plans *billing.Plans
	// Policies are read by every process: tunnels are throttled and new tunnels and uploads refused at the hard cap.
	Policies billing.PolicySource
	// Org and Operator serve the API; nil on device processes.
	Org      billing.OrgService
	Operator billing.OperatorService
	// Upkeep computes the policies and closes the periods, on one node at a time; nil on device processes.
	Upkeep BillingUpkeep
}

// BillingUpkeep is the billing upkeep (pgbilling.Upkeep).
type BillingUpkeep interface {
	Run(ctx context.Context) error
	SetNotifier(n billing.Notifier)
}

// billingUpkeepLease is the cluster lease of the billing upkeep.
const billingUpkeepLease = "billing-upkeep"

// setupBilling builds the enforcer and gives it to the tunnels and the local dispatcher.
func (s *Server) setupBilling(o *BillingOpts) {
	if o == nil || o.Policies == nil {
		return
	}
	s.enforcer = billing.NewEnforcer(o.Policies, o.Plans, logger.NewLogger("billing", s.config.Logging.LogOutput, s.config.Logging.LogLevel))
}

// billingAPI is the billing of the API listener; nil without [billing] or on device processes.
func (s *Server) billingAPI(o *BillingOpts) *BillingAPI {
	if o == nil || o.Org == nil {
		return nil
	}
	return &BillingAPI{Org: o.Org, Operator: o.Operator, Enforcer: s.enforcer}
}

// runBilling reads the transfer policies and, with an upkeep, runs it every billing.EnforceInterval under the
// cluster lease, until ctx ends.
func (s *Server) runBilling(ctx context.Context, o *BillingOpts) {
	if s.enforcer != nil {
		go s.enforcer.Run(ctx)
	}
	if o == nil || o.Upkeep == nil {
		return
	}
	if s.apiListener != nil && s.apiListener.notificationsStorage != nil {
		o.Upkeep.SetNotifier(billingNotifier{d: notifications.NewDispatcher(s.apiListener.notificationsStorage)})
	}
	log := s.Logger.Fork("billing")
	go func() {
		run := func() {
			held, err := s.cluster.Lease(ctx, billingUpkeepLease, "", leaseTTL(billing.EnforceInterval))
			if err != nil {
				log.Errorf("billing upkeep: lease: %v", err)
				return
			}
			if !held {
				return
			}
			if err := o.Upkeep.Run(tenant.WithID(ctx, "_system")); err != nil && ctx.Err() == nil {
				log.Errorf("%v", err)
			}
			// a new policy reaches this process at once, not at the next refresh
			if s.enforcer != nil {
				_ = s.enforcer.Refresh(ctx)
			}
		}
		run()
		tick := time.NewTicker(billing.EnforceInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				run()
			}
		}
	}()
}

// billingNotifier sends billing notifications as email through the server's notifications.
type billingNotifier struct {
	d notifications.Dispatcher
}

func (n billingNotifier) Notify(ctx context.Context, tid tenant.ID, to []string, subject, body string) error {
	_, err := n.d.Dispatch(tenant.WithID(ctx, tid), refs.GenerateIdentifiable("billing"), notifications.NotificationData{
		Target:      string(notifications.TargetMail),
		Recipients:  to,
		Subject:     subject,
		Content:     body,
		ContentType: notifications.ContentTypeTextPlain,
	})
	return err
}
