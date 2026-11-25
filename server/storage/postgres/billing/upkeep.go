package billing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/billing"
	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

// maxCloseBack is how many months back the upkeep closes periods it missed (a cluster down that long).
const maxCloseBack = 12

// upkeepActor is who the audit log names for the upkeep's changes.
var upkeepActor = billing.Actor{Username: "billing:upkeep"}

// Upkeep keeps every organization's billing (the server runs it every billing.EnforceInterval on one node, under a
// lease): it creates the missing records (free plan, paying), ends trials and comped periods, computes and writes
// the transfer policy of the current period, notifies the administrators at the soft cap, at 100% and at the hard
// cap and before a trial ends, and closes the months that ended more than billing.SettleDelay ago.
type Upkeep struct {
	DB    *sqlx.DB
	Plans *billing.Plans
	// Notifier (optional) sends the notifications.
	Notifier billing.Notifier
	Log      *logger.Logger
	// ReviewAboveCents holds closed periods with more overage for an operator's approval (0: none).
	ReviewAboveCents int64
	Enforce          billing.EnforceOptions
	// Now is time.Now unless set (tests).
	Now func() time.Time
}

// SetNotifier sets the Notifier (the server's notifications); call before Run.
func (u *Upkeep) SetNotifier(n billing.Notifier) { u.Notifier = n }

func (u *Upkeep) now() time.Time {
	if u.Now != nil {
		return u.Now().UTC()
	}
	return time.Now().UTC()
}

func (u *Upkeep) errorf(format string, a ...any) {
	if u.Log != nil {
		u.Log.Errorf(format, a...)
	}
}

// Run does one round for every organization; it returns the first error and goes on with the others.
func (u *Upkeep) Run(ctx context.Context) error {
	now := u.now()
	var rows []row
	var newest sql.NullTime
	err := inSystem(ctx, u.DB, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, "SELECT rport_billing_ensure($1)", u.Plans.Free); err != nil {
			return err
		}
		if err := tx.GetContext(ctx, &newest, "SELECT rport_billing_newest_usage()"); err != nil {
			return err
		}
		return tx.SelectContext(ctx, &rows, "SELECT "+accountColumns+" FROM rport_operator_billing(NULL)")
	})
	if err != nil {
		return fmt.Errorf("billing upkeep: %w", err)
	}
	var orgs []string
	if err := u.DB.SelectContext(ctx, &orgs, "SELECT id FROM tenants WHERE left(id, 1) <> '_' AND status IN ('active', 'suspended')"); err != nil {
		return fmt.Errorf("billing upkeep: %w", err)
	}
	exists := map[string]bool{}
	for _, id := range orgs {
		exists[id] = true
	}
	var firstErr error
	for i := range rows {
		r := &rows[i]
		if !exists[r.Organization] {
			continue // deleted: the record stays, detached
		}
		if err := u.organization(ctx, r, newest.Time, now); err != nil {
			u.errorf("billing upkeep: organization %s: %v", r.Organization, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (u *Upkeep) organization(ctx context.Context, r *row, newest, now time.Time) error {
	b := &r.Billing
	if err := u.transition(ctx, b, now); err != nil {
		return err
	}
	if err := u.trialNotices(ctx, b, now); err != nil {
		return err
	}
	cu, err := u.currentUsage(ctx, b, now)
	if err != nil {
		return err
	}
	cu.NewestUsage = newest
	if r.FailOpen != "" && r.FailOpenAt != nil && billing.MonthStart(*r.FailOpenAt).Equal(billing.MonthStart(now)) {
		cu.FailedOpen = r.FailOpen // held open for the rest of the period, until an operator clears it
	}
	if u.Plans.Get(b.Plan) == nil {
		u.errorf("billing upkeep: organization %s is on the plan %q, which the plans file doesn't have: the free plan's limits apply", b.Organization, b.Plan)
	}
	p := billing.Evaluate(b, u.Plans, cu, u.Enforce, now)
	if p.FailOpen != "" {
		u.errorf("billing upkeep: organization %s reached its hard cap, not applied (fail open): %s", b.Organization, p.FailOpen)
		// the operators are told once per period
		body := fmt.Sprintf("The organization %s reached its hard transfer cap (%s used of %s), but the cap was not applied: %s. "+
			"Check its usage; \"rportd org lift-cap %s --clear-fail-open\" lets the cap apply again.",
			b.Organization, billing.ByteSize(p.UsedBytes), billing.ByteSize(p.AllowanceBytes), p.FailOpen, b.Organization)
		if err := u.notice(ctx, string(systemTenant), billing.MonthStart(now), "fail_open:"+b.Organization,
			"Billing: hard cap not applied (fail open) for "+b.Organization, body); err != nil {
			u.errorf("billing upkeep: %v", err)
		}
	}
	err = inSystem(ctx, u.DB, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, "SELECT rport_billing_set_policy($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)",
			b.Organization, string(p.State), p.AllowanceBytes, p.UsedBytes, p.SoftCapBytes, p.HardCapBytes, p.OverageCents,
			int64(p.TunnelRate), int64(p.ThrottleRate), p.MaxClients, p.FailOpen)
		return err
	})
	if err != nil {
		return err
	}
	if err := u.transferNotices(ctx, b, p, now); err != nil {
		return err
	}
	return u.closePeriods(ctx, b, now)
}

// transition ends a trial (at trial_ends_at) or a comped status (the day after comped_until): paying, on the same
// plan with a payment method on file, on the free plan without one.
func (u *Upkeep) transition(ctx context.Context, b *billing.Billing, now time.Time) error {
	var action string
	switch {
	case b.Status == billing.StatusTrial && b.TrialEndsAt != nil && !now.Before(*b.TrialEndsAt):
		action = billing.AuditActionTrialEnd
	case b.Status == billing.StatusComped && b.CompedUntil != nil && now.After(billing.DayStart(*b.CompedUntil).AddDate(0, 0, 1)):
		action = billing.AuditActionCompedEnd
	default:
		return nil
	}
	next := *b
	next.Status = billing.StatusPaying
	if !b.HasPaymentMethod {
		next.Plan = u.Plans.Free
	}
	maxClients := billing.MaxClients(&next, u.Plans)
	var changed bool
	err := inSystem(ctx, u.DB, func(tx *sqlx.Tx) error {
		if err := tx.GetContext(ctx, &changed, "SELECT rport_billing_transition($1, $2, $3, $4)", b.Organization, string(b.Status), next.Plan, maxClients); err != nil || !changed {
			return err
		}
		return auditBoth(ctx, tx, upkeepActor, action, b.Organization, map[string]any{"status": next.Status, "plan": next.Plan, "max_clients": maxClients})
	})
	if err != nil {
		return err
	}
	if changed {
		*b = next
	}
	return nil
}

// trialNotices tells the administrators 14 and 3 days before a trial (or a comped status without a payment method)
// ends.
func (u *Upkeep) trialNotices(ctx context.Context, b *billing.Billing, now time.Time) error {
	var ends time.Time
	switch {
	case b.Status == billing.StatusTrial && b.TrialEndsAt != nil:
		ends = *b.TrialEndsAt
	case b.Status == billing.StatusComped && b.CompedUntil != nil && !b.HasPaymentMethod:
		ends = billing.DayStart(*b.CompedUntil).AddDate(0, 0, 1)
	default:
		return nil
	}
	left := ends.Sub(now)
	for _, days := range []int{3, 14} {
		if left > time.Duration(days)*24*time.Hour || left <= 0 {
			continue
		}
		then := "the organization moves to the free plan"
		if b.HasPaymentMethod {
			then = "the plan is charged from then on"
		}
		what := "trial"
		if b.Status == billing.StatusComped {
			what = "complimentary period"
		}
		subject := fmt.Sprintf("Your %s ends in %d days", what, days)
		body := fmt.Sprintf("Your %s of the plan %s ends on %s; then %s. Add a payment method to keep the plan.",
			what, u.planName(b.Plan), ends.Format("2006-01-02"), then)
		return u.notice(ctx, b.Organization, billing.DayStart(ends), fmt.Sprintf("trial_ending_%d", days), subject, body)
	}
	return nil
}

func (u *Upkeep) planName(id string) string {
	if pl := u.Plans.Get(id); pl != nil {
		return fmt.Sprintf("%q", pl.Name)
	}
	return fmt.Sprintf("%q", id)
}

// transferNotices sends one notification per state per period: at the soft cap, at 100% and at the hard cap.
func (u *Upkeep) transferNotices(ctx context.Context, b *billing.Billing, p billing.Policy, now time.Time) error {
	period := billing.MonthStart(now)
	used := billing.ByteSize(p.UsedBytes).String()
	allowance := billing.ByteSize(p.AllowanceBytes).String()
	var kind, subject, body string
	switch p.State {
	case billing.TransferSoft:
		kind, subject = "soft_cap", "Transfer: most of this month's allowance is used"
		body = fmt.Sprintf("Your organization used %s of its %s transfer allowance this month (plan %s). Nothing is limited yet.", used, allowance, u.planName(p.Plan))
	case billing.TransferOver:
		kind, subject = "allowance_reached", "Transfer: this month's allowance is used"
		then := "Transfer from now on is billed as overage."
		if pl := u.Plans.Get(p.Plan); (pl == nil || pl.Overage == billing.OverageCap) || b.CardlessTrial() {
			then = "Tunnels are slowed and new tunnels and uploads refused once the transfer limit is reached."
		}
		body = fmt.Sprintf("Your organization used %s, its whole %s transfer allowance this month (plan %s). %s", used, allowance, u.planName(p.Plan), then)
		if p.FailOpen != "" {
			return nil // reached the hard cap but not applied: the operators look into it first
		}
	case billing.TransferCapped:
		kind, subject = "hard_cap", "Transfer limit reached: tunnels are slowed"
		body = fmt.Sprintf("Your organization reached its transfer limit this month (%s used, plan %s). Tunnels are slowed and new tunnels and uploads are refused until the next month; devices stay connected and commands keep working. Choose a larger plan or raise the spending limit to lift it.", used, u.planName(p.Plan))
	default:
		return nil
	}
	return u.notice(ctx, b.Organization, period, kind, subject, body)
}

// notice claims (org, period, kind) and, when it is new, notifies the organization's administrators.
func (u *Upkeep) notice(ctx context.Context, org string, period time.Time, kind, subject, body string) error {
	var claimed bool
	err := inSystem(ctx, u.DB, func(tx *sqlx.Tx) error {
		return tx.GetContext(ctx, &claimed, "SELECT rport_billing_claim_notice($1, $2::date, $3)", org, period.Format("2006-01-02"), kind)
	})
	if err != nil || !claimed || u.Notifier == nil {
		return err
	}
	to, err := AdminRecipients(ctx, u.DB, tenant.ID(org))
	if err != nil {
		return err
	}
	if len(to) == 0 {
		return nil
	}
	if err := u.Notifier.Notify(tenant.WithID(ctx, tenant.ID(org)), tenant.ID(org), to, subject, body); err != nil {
		u.errorf("billing upkeep: notify %s (%s): %v", org, kind, err)
	}
	return nil
}

// AdminRecipients are the email addresses of tid's administrators: usernames and 2FA addresses that are emails.
func AdminRecipients(ctx context.Context, db *sqlx.DB, tid tenant.ID) ([]string, error) {
	var rows []struct {
		Username string `db:"username"`
		SendTo   string `db:"two_fa_send_to"`
	}
	err := postgres.InTenantID(ctx, db, tid, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &rows, `SELECT u.username, u.two_fa_send_to FROM users u
			JOIN user_groups g ON g.tenant_id = u.tenant_id AND g.username = u.username
			WHERE u.tenant_id = $1 AND g.group_name = 'Administrators' ORDER BY u.username`, string(tid))
	})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		for _, a := range []string{r.Username, r.SendTo} {
			a = strings.TrimSpace(a)
			if strings.Contains(a, "@") && !strings.ContainsAny(a, " \t\r\n") && !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	return out, nil
}

// spanUsage reads tid's transfer and daily device counts in [from, to) (whole UTC days).
func spanUsage(ctx context.Context, tx *sqlx.Tx, tid string, from, to time.Time) (billing.PeriodUsage, error) {
	var u billing.PeriodUsage
	var rows []struct {
		Metric string `db:"metric"`
		Value  int64  `db:"value"`
	}
	if err := tx.SelectContext(ctx, &rows, `SELECT metric, sum(value)::bigint AS value FROM usage_periods
		WHERE tenant_id = $1 AND hour >= $2 AND hour < $3
			AND metric IN ('tunnel_bytes_to_user', 'tunnel_bytes_to_device', 'upload_bytes_to_devices')
		GROUP BY metric`, tid, from, to); err != nil {
		return u, err
	}
	for _, r := range rows {
		switch r.Metric {
		case "tunnel_bytes_to_user":
			u.TunnelBytesToUser = r.Value
		case "tunnel_bytes_to_device":
			u.TunnelBytesToDevice = r.Value
		case "upload_bytes_to_devices":
			u.UploadBytesToDevices = r.Value
		}
	}
	err := tx.SelectContext(ctx, &u.DailyDevices, `SELECT count(*) FROM usage_device_days
		WHERE tenant_id = $1 AND day >= $2::date AND day < $3::date GROUP BY day ORDER BY day`,
		tid, from.Format("2006-01-02"), to.Format("2006-01-02"))
	return u, err
}

// currentUsage reads what b's organization used in the current period so far.
func (u *Upkeep) currentUsage(ctx context.Context, b *billing.Billing, now time.Time) (billing.CurrentUsage, error) {
	from := billing.MonthStart(now)
	if s := billing.DayStart(b.StartedAt); s.After(from) {
		from = s
	}
	tomorrow := billing.DayStart(now).AddDate(0, 0, 1)
	lastHour := now.Truncate(time.Hour).Add(-time.Hour)
	var cu billing.CurrentUsage
	err := postgres.InTenantID(ctx, u.DB, tenant.ID(b.Organization), func(tx *sqlx.Tx) error {
		su, err := spanUsage(ctx, tx, b.Organization, from, tomorrow)
		if err != nil {
			return err
		}
		cu.TransferBytes = su.TunnelBytesToUser + su.TunnelBytesToDevice + su.UploadBytesToDevices
		for _, n := range su.DailyDevices {
			cu.DeviceDays += n
		}
		// today counts once its devices were reported (they arrive with the first flush after midnight)
		today := billing.DayStart(now)
		var todays int64
		if err := tx.GetContext(ctx, &todays, "SELECT count(*) FROM usage_device_days WHERE tenant_id = $1 AND day = $2::date",
			b.Organization, today.Format("2006-01-02")); err != nil {
			return err
		}
		end := tomorrow
		if todays == 0 {
			end = today
		}
		cu.Days = int64(end.Sub(from).Hours()+0.5) / 24
		if cu.Days <= 0 {
			// the first day of the period, not reported yet: yesterday's devices
			if err := tx.GetContext(ctx, &cu.DeviceDays, "SELECT count(*) FROM usage_device_days WHERE tenant_id = $1 AND day = $2::date",
				b.Organization, today.AddDate(0, 0, -1).Format("2006-01-02")); err != nil {
				return err
			}
			cu.Days = 1
		}
		var before int64
		if err := tx.QueryRowxContext(ctx, `SELECT
				coalesce(sum(value) FILTER (WHERE hour = $3), 0)::bigint,
				coalesce(sum(value) FILTER (WHERE hour >= $2 AND hour < $3), 0)::bigint
			FROM usage_hourly WHERE tenant_id = $1 AND hour >= $2 AND hour <= $3
				AND metric IN ('tunnel_bytes_to_user', 'tunnel_bytes_to_device', 'upload_bytes_to_devices')`,
			b.Organization, from, lastHour).Scan(&cu.LastHourBytes, &before); err != nil {
			return err
		}
		if hours := lastHour.Sub(from).Hours(); hours >= 1 {
			cu.AvgHourBytes = float64(before) / hours
		}
		return nil
	})
	return cu, err
}

// closePeriods closes the months of b that ended more than billing.SettleDelay ago and are not closed yet.
func (u *Upkeep) closePeriods(ctx context.Context, b *billing.Billing, now time.Time) error {
	last := billing.MonthStart(now.Add(-billing.SettleDelay)).AddDate(0, -1, 0) // the newest month to close
	first := billing.MonthStart(b.StartedAt)
	if back := last.AddDate(0, -maxCloseBack+1, 0); first.Before(back) {
		first = back
	}
	if last.Before(first) {
		return nil
	}
	var closed []time.Time
	err := postgres.InTenantID(ctx, u.DB, tenant.ID(b.Organization), func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &closed, "SELECT (period_start::timestamp AT TIME ZONE 'UTC') FROM billing_periods WHERE tenant_id = $1 AND period_start >= $2::date",
			b.Organization, first.Format("2006-01-02"))
	})
	if err != nil {
		return err
	}
	done := map[time.Time]bool{}
	for _, c := range closed {
		done[c.UTC()] = true
	}
	for m := first; !m.After(last); m = m.AddDate(0, 1, 0) {
		if done[m] {
			continue
		}
		// closed with the status and plan that applied at the end of the month
		at := b.AsOf(m.AddDate(0, 1, 0))
		plan := billing.EffectivePlan(&at, u.Plans)
		from, _ := billing.PeriodSpan(&at, m)
		var pu billing.PeriodUsage
		err := postgres.InTenantID(ctx, u.DB, tenant.ID(b.Organization), func(tx *sqlx.Tx) (err error) {
			pu, err = spanUsage(ctx, tx, b.Organization, from, m.AddDate(0, 1, 0))
			return err
		})
		if err != nil {
			return err
		}
		p := billing.ClosePeriod(&at, plan, m, pu, u.ReviewAboveCents, now)
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		err = inSystem(ctx, u.DB, func(tx *sqlx.Tx) error {
			var inserted bool
			return tx.GetContext(ctx, &inserted, "SELECT rport_billing_close_period($1)", string(raw))
		})
		if err != nil {
			return err
		}
		if p.State == billing.PeriodHeld && u.Log != nil {
			u.Log.Infof("billing: period %s of %s is held for review: overage %d cents", m.Format("2006-01"), b.Organization, p.OverageCents)
		}
	}
	return nil
}
