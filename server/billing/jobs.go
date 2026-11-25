package billing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// LeaseFunc takes or renews the cluster-wide lease name for ttl (one node runs each job at a time).
type LeaseFunc func(ctx context.Context, name string, ttl time.Duration) (bool, error)

// Lease names of the jobs.
const (
	LeaseReport    = "billing-report"
	LeaseReconcile = "billing-reconcile"
	LeaseDunning   = "billing-dunning"
)

// Run runs the reporting, reconciliation and dunning jobs until ctx is done, each under its lease.
func (s *Service) Run(ctx context.Context, lease LeaseFunc) {
	type job struct {
		name     string
		interval time.Duration
		run      func(context.Context) error
	}
	jobs := []job{
		{LeaseReport, ReportInterval, s.ReportClosedPeriods},
		{LeaseReconcile, ReconcileInterval, s.Reconcile},
		{LeaseDunning, DunningInterval, s.DunningTick},
	}
	for _, j := range jobs {
		go func() {
			once := func() {
				ok, err := lease(ctx, j.name, j.interval+j.interval/2)
				if err != nil {
					s.log.Errorf("billing: lease %s: %v", j.name, err)
					return
				}
				if !ok {
					return
				}
				if err := j.run(ctx); err != nil && ctx.Err() == nil {
					s.log.Errorf("billing: %s: %v", j.name, err)
				}
			}
			once()
			tick := time.NewTicker(j.interval)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					once()
				}
			}
		}()
	}
}

type reportKey struct {
	org    string
	start  int64
	metric string
}

func keyOf(org string, start time.Time, metric string) reportKey {
	return reportKey{org: org, start: start.UTC().Unix(), metric: metric}
}

// ReportClosedPeriods reports the quantities of every closed period that is ready to be charged: one record per
// organization, metric and period (extra devices, overage GiB), identified by UsageIdentifier, timestamped in the
// period's last second. Nothing is sent for exempt organizations (checked on the period and on the current
// record), for held or uncharged periods, or for quantities of 0. A period whose every quantity is sent is marked
// reported.
func (s *Service) ReportClosedPeriods(ctx context.Context) error {
	if s.o.Periods == nil {
		return nil
	}
	now := s.now()
	periods, err := s.o.Periods.ClosedSince(ctx, now.Add(-reportLookback))
	if err != nil {
		return err
	}
	byStart := map[int64][]UsageReport{}
	var errs []error
	for _, p := range periods {
		if p.State != PeriodReady || p.Status == StatusExempt {
			continue
		}
		b, err := s.record(ctx, p.Organization)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if b.Status == StatusExempt {
			continue
		}
		existing, ok := byStart[p.Start.Unix()]
		if !ok {
			if existing, err = s.o.Store.Reports(ctx, p.Start); err != nil {
				return err
			}
			byStart[p.Start.Unix()] = existing
		}
		done, err := s.reportPeriod(ctx, p, existing, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %s: %w", p.Organization, p.Start.Format("2006-01"), err))
			continue
		}
		if done && s.o.Sync != nil {
			if err := s.o.Sync.PeriodReported(ctx, p.Organization, p.Start, now); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// reportPeriod sends p's quantities not sent yet; done when every quantity is sent.
func (s *Service) reportPeriod(ctx context.Context, p Period, existing []UsageReport, now time.Time) (done bool, err error) {
	plan := s.o.Plans.Get(p.Plan)
	if plan == nil {
		return false, fmt.Errorf("unknown plan %q", p.Plan)
	}
	a, err := s.o.Store.Account(ctx, p.Organization)
	if err != nil {
		return false, err
	}
	if a == nil || a.CustomerID == "" {
		return false, errors.New("the period is to be charged but the organization has no provider customer")
	}
	have := map[reportKey]UsageReport{}
	for _, r := range existing {
		have[keyOf(r.Organization, r.PeriodStart, r.Metric)] = r
	}
	done = true
	for _, m := range []struct {
		metric string
		qty    int64
	}{{MetricExtraDevices, p.ExtraDevices}, {MetricOverageGiB, p.OverageGiB}} {
		if m.qty <= 0 {
			continue
		}
		price := plan.ProviderPrices[PriceKeyOf(m.metric)]
		if price == "" {
			done = false
			s.log.Errorf("billing: plan %q has no provider price for %s: %d not reported for %s", plan.ID, m.metric, m.qty, p.Organization)
			continue
		}
		r, ok := have[keyOf(p.Organization, p.Start, m.metric)]
		if !ok {
			r = UsageReport{Organization: p.Organization, PeriodStart: p.Start.UTC(), PeriodEnd: p.End.UTC(), Metric: m.metric,
				Identifier: UsageIdentifier(p.Organization, m.metric, p.Start), CustomerID: a.CustomerID, PriceID: price,
				Quantity: m.qty, State: ReportFailed}
		}
		switch r.State {
		case ReportSent, ReportConfirmed, ReportMismatch:
			continue
		}
		if r.LastAttemptAt != nil && now.Sub(*r.LastAttemptAt) < retryAfter {
			done = false
			continue
		}
		if r.LastAttemptAt != nil && now.Sub(*r.LastAttemptAt) > dedupWindow {
			// a send that failed long ago may have reached the provider; its identifier no longer protects a
			// resend, so look first
			got, err := s.summary(ctx, r)
			if err != nil {
				done = false
				s.log.Errorf("billing: %v", err)
				continue
			}
			if got > 0 {
				r.State = ReportSent
				r.SentAt = r.LastAttemptAt
				if err := s.o.Store.PutReport(ctx, &r); err != nil {
					return false, err
				}
				continue
			}
		}
		if err := s.send(ctx, &r, now); err != nil {
			done = false
			s.log.Errorf("billing: report %s: %v", r.Identifier, err)
		}
	}
	return done, nil
}

// send sends r and records the outcome.
func (s *Service) send(ctx context.Context, r *UsageReport, now time.Time) error {
	r.Attempts++
	r.LastAttemptAt = &now
	sendErr := s.o.Provider.ReportUsage(ctx, UsageRecord{Identifier: r.Identifier, CustomerID: r.CustomerID, PriceID: r.PriceID,
		Metric: r.Metric, Quantity: r.Quantity, At: r.PeriodEnd.Add(-time.Second)})
	if sendErr != nil {
		r.State = ReportFailed
		r.LastError = truncate(sendErr.Error(), 500)
		s.count(func(st *Stats) { st.ReportsFailed++ })
	} else {
		r.State = ReportSent
		r.SentAt = &now
		r.LastError = ""
		s.count(func(st *Stats) { st.ReportsSent++ })
	}
	if err := s.o.Store.PutReport(ctx, r); err != nil {
		return errors.Join(sendErr, err)
	}
	return sendErr
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (s *Service) summary(ctx context.Context, r UsageReport) (float64, error) {
	return s.o.Provider.UsageSummary(ctx, UsageQuery{CustomerID: r.CustomerID, PriceID: r.PriceID, From: r.PeriodStart, To: r.PeriodEnd})
}

// Reconcile compares every sent report with the provider's summary of its period: equal is confirmed; nothing
// recorded after resendAfter is sent again (same identifier; the summary shows the first one never counted);
// anything else is a mismatch for operators (no automatic correction).
func (s *Service) Reconcile(ctx context.Context) error {
	reports, err := s.o.Store.Reports(ctx, time.Time{})
	if err != nil {
		return err
	}
	now := s.now()
	var errs []error
	for _, r := range reports {
		if r.State != ReportSent || r.SentAt == nil || now.Sub(*r.SentAt) < confirmAfter {
			continue
		}
		got, err := s.summary(ctx, r)
		if err != nil {
			errs = append(errs, fmt.Errorf("summary of %s: %w", r.Identifier, err))
			continue
		}
		switch {
		case math.Abs(got-float64(r.Quantity)) < 1e-9:
			r.State = ReportConfirmed
			r.ConfirmedAt = &now
			r.ProviderValue = &got
		case got == 0 && r.LastAttemptAt != nil && now.Sub(*r.LastAttemptAt) >= resendAfter:
			s.log.Infof("billing: the provider shows nothing for %s; sending it again", r.Identifier)
			if err := s.send(ctx, &r, now); err != nil {
				errs = append(errs, err)
			}
			continue
		case got == 0:
			continue
		default:
			r.State = ReportMismatch
			r.ProviderValue = &got
			s.count(func(st *Stats) { st.Mismatches++ })
			s.log.Errorf("billing: reconciliation: %s is %d here but %g at the provider; operators must look", r.Identifier, r.Quantity, got)
		}
		if err := s.o.Store.PutReport(ctx, &r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// DunningTick moves overdue organizations on: read-only after the grace period, suspension proposed after the
// read-only period; their administrators are notified at each step.
func (s *Service) DunningTick(ctx context.Context) error {
	accounts, err := s.o.Store.Accounts(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	var errs []error
	for i := range accounts {
		a := accounts[i]
		next := a.Tick(now, s.o.Dunning)
		if next.State == a.State {
			continue
		}
		from := a.State
		a.Dunning = next
		a.UpdatedBy = "billing:dunning"
		if err := s.o.Store.PutAccount(ctx, &a); err != nil {
			errs = append(errs, err)
			continue
		}
		s.forget(a.Organization)
		if err := s.o.Store.Audit(ctx, Actor{Username: "billing:dunning"}, AuditDunning, a.Organization,
			map[string]string{"from": string(from), "to": string(next.State)}); err != nil {
			s.log.Errorf("billing: %v", err)
		}
		switch next.State {
		case DunningReadOnly:
			s.notifyOnce(ctx, a.Organization, "read_only", episode(&a), "Organization is read-only",
				fmt.Sprintf("The payment for organization %s is still overdue, so it is read-only: you can log in and see everything, "+
					"but no new jobs, tunnels, uploads or changes are possible. Devices stay connected. Pay the open invoice "+
					"on the Billing page%s to restore everything at once.", a.Organization, s.billingLink()))
		case DunningSuspensionProposed:
			s.log.Errorf("billing: organization %s has been read-only for %d days; suspension proposed (an operator must confirm)",
				a.Organization, int(s.o.Dunning.ReadOnly.Hours()/24))
			s.notifyOnce(ctx, a.Organization, "suspension_proposed", episode(&a), "Organization may be suspended",
				fmt.Sprintf("The payment for organization %s is still overdue. The organization may be suspended: users won't be able to "+
					"log in and devices won't be able to connect. Pay the open invoice on the Billing page%s.", a.Organization, s.billingLink()))
		}
	}
	return errors.Join(errs...)
}

// ---- operators ----

// AccountView is an organization's provider account for operators, with its usage reports.
type AccountView struct {
	ProviderAccount
	Access             Access `json:"access"`
	SuspensionProposed bool   `json:"suspension_proposed"`
}

// Accounts lists every organization's provider account for operators.
func (s *Service) Accounts(ctx context.Context) ([]AccountView, error) {
	all, err := s.o.Store.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]AccountView, 0, len(all))
	for _, a := range all {
		if a.State == "" {
			a.State = DunningNone
		}
		out = append(out, AccountView{ProviderAccount: a, Access: a.Access(), SuspensionProposed: a.State == DunningSuspensionProposed})
	}
	return out, nil
}

// Reports lists the usage reports (every period when start is zero) for operators.
func (s *Service) Reports(ctx context.Context, start time.Time) ([]UsageReport, error) {
	return s.o.Store.Reports(ctx, start)
}

func (s *Service) operatorAccount(ctx context.Context, org string) (*ProviderAccount, error) {
	a, err := s.o.Store.Account(ctx, org)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, fmt.Errorf("%w: organization %q has no provider account", ErrNotFound, org)
	}
	return a, nil
}

// ConfirmSuspension suspends org (operator-confirmed, decision 4) when its suspension is proposed. The dunning
// state stays: a later payment restores access, and an operator activates the organization again.
func (s *Service) ConfirmSuspension(ctx context.Context, by Actor, org string) error {
	if s.o.Suspender == nil {
		return errors.New("billing: suspension is not available")
	}
	a, err := s.operatorAccount(ctx, org)
	if err != nil {
		return err
	}
	if err := a.CanConfirmSuspension(); err != nil {
		return err
	}
	if err := s.o.Suspender.Suspend(ctx, by, org); err != nil {
		return err
	}
	return s.o.Store.Audit(ctx, by, AuditConfirmSuspension, org, map[string]string{"failed_invoice": a.FailedInvoice})
}

// Restore moves org out of the dunning states without a payment (operator override, audited).
func (s *Service) Restore(ctx context.Context, by Actor, org string) (*ProviderAccount, error) {
	a, err := s.operatorAccount(ctx, org)
	if err != nil {
		return nil, err
	}
	from := a.State
	a.Dunning = a.Restore()
	a.UpdatedBy = by.Username
	if err := s.o.Store.PutAccount(ctx, a); err != nil {
		return nil, err
	}
	s.forget(org)
	if err := s.o.Store.Audit(ctx, by, AuditRestore, org, map[string]string{"from": string(from)}); err != nil {
		return nil, err
	}
	return a, nil
}
