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
// organization, metric and period (extra devices, prorated like the period's device charge, and overage GiB),
// identified by UsageIdentifier and timestamped in the period's last second — or, when the period's invoice has
// finalized already (a period approved late, a long provider outage), now, so it lands on the next invoice instead
// of nowhere. Nothing is sent for exempt organizations (checked on the period and on the current record), for held
// or uncharged periods, or for quantities of 0. A period whose every quantity is sent is marked reported.
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

// quantities are the usage records of p: extra devices prorated by the share of the month billed (as the period's
// device charge is), overage in whole GiB.
func quantities(p Period) []struct {
	metric string
	qty    float64
} {
	devices := float64(p.ExtraDevices)
	if p.Proration > 0 && p.Proration < 1 {
		devices = math.Round(devices*p.Proration*1e6) / 1e6
	}
	return []struct {
		metric string
		qty    float64
	}{{MetricExtraDevices, devices}, {MetricOverageGiB, float64(p.OverageGiB)}}
}

// reportPeriod sends p's quantities not sent yet; done when every quantity is sent (or expired).
func (s *Service) reportPeriod(ctx context.Context, p Period, existing []UsageReport, now time.Time) (done bool, err error) {
	have := map[reportKey]UsageReport{}
	for _, r := range existing {
		have[keyOf(r.Organization, r.PeriodStart, r.Metric)] = r
	}
	var plan *Plan
	var a *ProviderAccount
	done = true
	for _, m := range quantities(p) {
		if m.qty <= 0 {
			continue
		}
		if plan == nil {
			if plan = s.o.Plans.Get(p.Plan); plan == nil {
				return false, fmt.Errorf("unknown plan %q", p.Plan)
			}
			if a, err = s.o.Store.Account(ctx, p.Organization); err != nil {
				return false, err
			}
			if a == nil || a.CustomerID == "" {
				return false, errors.New("the period is to be charged but the organization has no provider customer")
			}
		}
		price := plan.ProviderPrices[PriceKeyOf(m.metric)]
		if price == "" {
			done = false
			s.log.Errorf("billing: plan %q has no provider price for %s: %g not reported for %s", plan.ID, m.metric, m.qty, p.Organization)
			continue
		}
		r, ok := have[keyOf(p.Organization, p.Start, m.metric)]
		if !ok {
			r = UsageReport{Organization: p.Organization, PeriodStart: p.Start.UTC(), PeriodEnd: p.End.UTC(), Metric: m.metric,
				Identifier: UsageIdentifier(p.Organization, m.metric, p.Start), CustomerID: a.CustomerID, PriceID: price,
				Quantity: m.qty, State: ReportFailed}
		}
		switch r.State {
		case ReportSent, ReportConfirmed, ReportMismatch, ReportExpired:
			continue
		}
		if r.LastAttemptAt != nil && now.Sub(*r.LastAttemptAt) < retryAfter {
			done = false
			continue
		}
		if r.FirstAttemptAt != nil && now.Sub(*r.FirstAttemptAt) > dedupWindow && r.RecordedAt != nil {
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
			if r.State != ReportExpired {
				done = false
			}
			s.log.Errorf("billing: report %s: %v", r.Identifier, err)
		}
	}
	return done, nil
}

// usageTime is when r's usage is recorded at the provider: the period's last second, or now once the period's
// invoice finalized (the usage then goes on the next invoice). A report keeps the time of its first send, so a
// resend has the same identifier and time.
func (s *Service) usageTime(r *UsageReport, now time.Time) time.Time {
	if r.RecordedAt != nil {
		return *r.RecordedAt
	}
	at := r.PeriodEnd.Add(-time.Second)
	if now.After(r.PeriodEnd.Add(s.o.FinalizationGrace - finalizationMargin)) {
		at = now.Truncate(time.Second)
		s.log.Errorf("billing: %s is reported after its invoice finalized; it goes on the next invoice", r.Identifier)
	}
	return at
}

// send sends r and records the outcome.
func (s *Service) send(ctx context.Context, r *UsageReport, now time.Time) error {
	at := s.usageTime(r, now)
	r.Attempts++
	r.LastAttemptAt = &now
	if r.FirstAttemptAt == nil {
		r.FirstAttemptAt = &now
	}
	r.RecordedAt = &at
	sendErr := s.o.Provider.ReportUsage(ctx, UsageRecord{Identifier: r.Identifier, CustomerID: r.CustomerID, PriceID: r.PriceID,
		Metric: r.Metric, Quantity: r.Quantity, At: at})
	switch {
	case errors.Is(sendErr, ErrUsageTooOld):
		r.State = ReportExpired
		r.LastError = truncate(sendErr.Error(), 500)
		s.count(func(st *Stats) { st.ReportsFailed++ })
		s.log.Errorf("billing: %s (%g) is too old for the provider; operators must invoice it by hand", r.Identifier, r.Quantity)
	case sendErr != nil:
		r.State = ReportFailed
		r.LastError = truncate(sendErr.Error(), 500)
		s.count(func(st *Stats) { st.ReportsFailed++ })
	default:
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

// summary is what the provider recorded on r's meter in the minute r's usage was recorded at: one record per
// organization, metric and period, so nothing else of the customer falls in that minute.
func (s *Service) summary(ctx context.Context, r UsageReport) (float64, error) {
	at := r.PeriodEnd.Add(-time.Second)
	if r.RecordedAt != nil {
		at = *r.RecordedAt
	}
	from := at.Truncate(time.Minute)
	return s.o.Provider.UsageSummary(ctx, UsageQuery{CustomerID: r.CustomerID, PriceID: r.PriceID, From: from, To: from.Add(time.Minute)})
}

// Reconcile compares every sent report with the provider's summary: equal is confirmed; nothing recorded after
// resendAfter is sent again (same identifier and time; the summary shows the first one never counted); anything
// else is a mismatch for operators (no automatic correction).
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
		case math.Abs(got-r.Quantity) < 1e-6:
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
			s.log.Errorf("billing: reconciliation: %s is %g here but %g at the provider; operators must look", r.Identifier, r.Quantity, got)
		}
		if err := s.o.Store.PutReport(ctx, &r); err != nil {
			errs = append(errs, err)
		}
	}
	if s.o.PruneEvents != nil {
		if err := s.o.PruneEvents(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// DunningTick moves overdue organizations on: read-only after the grace period, suspension proposed after the
// read-only period; their administrators are notified at each step. Before a step, the failed invoice is looked up
// at the provider: paid or void (an event that never reached us) ends the episode. Exempt organizations are never
// in dunning, and a live subscription of an exempt organization is canceled (exempt is never charged).
func (s *Service) DunningTick(ctx context.Context) error {
	accounts, err := s.o.Store.Accounts(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	var errs []error
	for i := range accounts {
		a := accounts[i]
		b, err := s.record(ctx, a.Organization)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if b.Status == StatusExempt {
			if err := s.exemptCleanup(ctx, &a); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if a.Tick(now, s.o.Dunning).State == a.State {
			continue
		}
		resolved := false
		if a.FailedInvoice != "" {
			st, err := s.o.Provider.InvoiceStatus(ctx, a.FailedInvoice)
			if err != nil {
				errs = append(errs, err)
				continue // don't restrict on what we couldn't check
			}
			resolved = st == "paid" || st == "void"
		}
		var from, to DunningState
		err = s.o.Store.UpdateAccount(ctx, a.Organization, func(cur *ProviderAccount) (*ProviderAccount, error) {
			if cur == nil {
				return nil, nil
			}
			from = cur.State
			if resolved && cur.FailedInvoice == a.FailedInvoice {
				cur.Dunning = cur.PaymentSucceeded()
			} else {
				cur.Dunning = cur.Tick(now, s.o.Dunning)
			}
			to = cur.State
			if to == from {
				return nil, nil
			}
			cur.UpdatedBy = "billing:dunning"
			return cur, nil
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if to == from {
			continue
		}
		s.forget(a.Organization)
		if err := s.o.Store.Audit(ctx, Actor{Username: "billing:dunning"}, AuditDunning, a.Organization,
			map[string]string{"from": string(from), "to": string(to)}); err != nil {
			s.log.Errorf("billing: %v", err)
		}
		switch to {
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

// exemptCleanup ends what an exempt organization must not have: a dunning episode and a live subscription.
func (s *Service) exemptCleanup(ctx context.Context, a *ProviderAccount) error {
	if a.SubscriptionID != "" && live(a.SubscriptionStatus) {
		s.log.Errorf("billing: organization %s is exempt but has the live subscription %s: canceling it", a.Organization, a.SubscriptionID)
		if err := s.o.Provider.Cancel(ctx, a.SubscriptionID, false); err != nil {
			return err
		}
		if err := s.o.Store.Audit(ctx, Actor{Username: "billing:exempt"}, AuditCancel, a.Organization,
			map[string]string{"subscription": a.SubscriptionID}); err != nil {
			s.log.Errorf("billing: %v", err)
		}
	}
	if a.State == DunningNone || a.State == "" {
		return nil
	}
	err := s.o.Store.UpdateAccount(ctx, a.Organization, func(cur *ProviderAccount) (*ProviderAccount, error) {
		if cur == nil || cur.State == DunningNone {
			return nil, nil
		}
		cur.Dunning = cur.Restore()
		cur.UpdatedBy = "billing:exempt"
		return cur, nil
	})
	s.forget(a.Organization)
	return err
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
	err = s.o.Store.UpdateAccount(ctx, org, func(cur *ProviderAccount) (*ProviderAccount, error) {
		if cur == nil {
			return nil, ErrNotFound
		}
		from = cur.State
		cur.Dunning = cur.Restore()
		cur.UpdatedBy = by.Username
		a = cur
		return cur, nil
	})
	if err != nil {
		return nil, err
	}
	s.forget(org)
	if err := s.o.Store.Audit(ctx, by, AuditRestore, org, map[string]string{"from": string(from)}); err != nil {
		return nil, err
	}
	return a, nil
}
