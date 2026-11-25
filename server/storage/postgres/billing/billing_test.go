package billing

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/billing"
	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	"github.com/openrport/openrport/share/tenant"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var ctx = context.Background()

const plansFile = `
free_plan = "free"
[trial]
  plan = "team"
  days = 14
  max_clients = 25
  transfer = "25GiB"
[[plan]]
  id = "free"
  name = "Free"
  max_clients = 10
  transfer_base = "10GiB"
  overage = "cap"
  tunnel_rate_limit = "2Mbit"
  throttle_rate = "256kbit"
[[plan]]
  id = "team"
  name = "Team"
  base_price = 49.00
  included_devices = 50
  price_per_device = 0.80
  transfer_base = "250GiB"
  transfer_per_device = "5GiB"
  overage = "bill"
  overage_price_per_gib = 0.20
`

type env struct {
	d     *pgtest.DB
	plans *billing.Plans
	op    *Operator
	store Store
	up    *Upkeep
	clock time.Time
	sent  *notes
}

type note struct {
	tid     tenant.ID
	to      []string
	subject string
}

type notes struct {
	mu   sync.Mutex
	list []note
}

func (n *notes) Notify(_ context.Context, tid tenant.ID, to []string, subject, _ string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.list = append(n.list, note{tid, to, subject})
	return nil
}

func (n *notes) subjects(tid tenant.ID) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, x := range n.list {
		if x.tid == tid {
			out = append(out, x.subject)
		}
	}
	return out
}

func newEnv(t *testing.T, tenants ...tenant.ID) *env {
	d := pgtest.New(t, tenants...)
	p, err := billing.ParsePlans([]byte(plansFile))
	require.NoError(t, err)
	e := &env{d: d, plans: p, sent: &notes{}, clock: time.Date(2026, 11, 24, 12, 30, 0, 0, time.UTC)}
	now := func() time.Time { return e.clock }
	e.op = &Operator{DB: d.App, Plans: p, Now: now}
	e.store = Store{DB: d.App, Plans: p}
	e.up = &Upkeep{DB: d.App, Plans: p, Notifier: e.sent, Now: now, ReviewAboveCents: 10000}
	return e
}

var op = billing.Actor{Username: "ops@example.com", RemoteIP: "192.0.2.1"}

func (e *env) usage(t *testing.T, tid tenant.ID, hour time.Time, metric string, v int64) {
	t.Helper()
	_, err := e.d.Owner.Exec(`INSERT INTO usage_hourly (tenant_id, hour, metric, value) VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, hour, metric) DO UPDATE SET value = usage_hourly.value + EXCLUDED.value`, string(tid), hour, metric, v)
	require.NoError(t, err)
}

func (e *env) devices(t *testing.T, tid tenant.ID, from time.Time, days, n int) {
	t.Helper()
	for i := 0; i < days; i++ {
		for c := 0; c < n; c++ {
			_, err := e.d.Owner.Exec("INSERT INTO usage_device_days (tenant_id, day, client_id) VALUES ($1, $2::date, $3) ON CONFLICT DO NOTHING",
				string(tid), from.AddDate(0, 0, i).Format("2006-01-02"), "c"+string(rune('a'+c%26))+string(rune('a'+c/26)))
			require.NoError(t, err)
		}
	}
}

func (e *env) admin(t *testing.T, tid tenant.ID, username string) {
	t.Helper()
	_, err := e.d.Owner.Exec("INSERT INTO users (tenant_id, username, two_fa_send_to) VALUES ($1, $2, '')", string(tid), username)
	require.NoError(t, err)
	_, err = e.d.Owner.Exec("INSERT INTO user_groups (tenant_id, username, group_name) VALUES ($1, $2, 'Administrators')", string(tid), username)
	require.NoError(t, err)
}

func (e *env) startedAt(t *testing.T, tid tenant.ID, at time.Time) {
	t.Helper()
	_, err := e.d.Owner.Exec("UPDATE org_billing SET started_at = $2 WHERE tenant_id = $1", string(tid), at)
	require.NoError(t, err)
}

func maxClients(t *testing.T, e *env, tid tenant.ID) *int {
	t.Helper()
	var n *int
	require.NoError(t, e.d.Owner.Get(&n, "SELECT max_clients FROM tenants WHERE id = $1", string(tid)))
	return n
}

func auditActions(t *testing.T, e *env, tid tenant.ID) []string {
	t.Helper()
	var out []string
	require.NoError(t, e.d.Owner.Select(&out, "SELECT action FROM auditlog WHERE tenant_id = $1 AND application = 'billing' ORDER BY id", string(tid)))
	return out
}

func TestOperatorSetsPlanAndStatusAudited(t *testing.T) {
	e := newEnv(t, "acme", "beta")
	a, err := e.op.Account(ctx, "acme")
	require.NoError(t, err)
	assert.Equal(t, "free", a.Plan, "no record yet: the free plan")
	assert.Equal(t, billing.StatusPaying, a.Status)

	a, err = e.op.SetPlan(ctx, op, "acme", "team")
	require.NoError(t, err)
	assert.Equal(t, "team", a.Plan)
	assert.Equal(t, "Team", a.PlanDetails.Name)
	assert.Nil(t, maxClients(t, e, "acme"), "Team has no device limit")
	_, err = e.op.SetPlan(ctx, op, "acme", "free")
	require.NoError(t, err)
	assert.Equal(t, 10, *maxClients(t, e, "acme"))

	_, err = e.op.SetPlan(ctx, op, "acme", "gold")
	assert.ErrorIs(t, err, billing.ErrInvalid)
	_, err = e.op.SetPlan(ctx, op, "nope", "team")
	assert.ErrorIs(t, err, billing.ErrNotFound)
	_, err = e.op.SetPlan(ctx, op, "_system", "team")
	assert.ErrorIs(t, err, billing.ErrNotFound)

	a, err = e.op.SetStatus(ctx, op, "acme", billing.StatusChange{Status: billing.StatusExempt, Reason: "partner", ExemptLimits: billing.ExemptLimitsNone})
	require.NoError(t, err)
	assert.Equal(t, billing.StatusExempt, a.Status)
	assert.Equal(t, "partner", a.Reason)
	assert.Equal(t, billing.ExemptLimitsNone, a.ExemptLimits)
	until := e.clock.AddDate(0, 4, 0)
	a, err = e.op.SetStatus(ctx, op, "beta", billing.StatusChange{Status: billing.StatusComped, CompedUntil: &until})
	require.NoError(t, err)
	assert.Equal(t, billing.DayStart(until), a.CompedUntil.UTC())
	_, err = e.op.SetStatus(ctx, op, "beta", billing.StatusChange{Status: billing.StatusExempt})
	assert.ErrorIs(t, err, billing.ErrInvalid)

	assert.Equal(t, []string{"set_plan", "set_plan", "set_status"}, auditActions(t, e, "acme"), "the organization's own audit log")
	assert.Len(t, auditActions(t, e, "_system"), 4)

	all, err := e.op.Accounts(ctx)
	require.NoError(t, err)
	ids := []string{}
	for _, a := range all {
		ids = append(ids, a.Organization)
	}
	assert.Contains(t, ids, "acme")
	assert.Contains(t, ids, "beta")
	assert.NotContains(t, ids, "_system")
}

func TestBillingTablesAreWrittenOnlyThroughTheFunctions(t *testing.T) {
	e := newEnv(t, "acme")
	_, err := e.op.SetPlan(ctx, op, "acme", "team")
	require.NoError(t, err)
	err = postgres.InTenantID(ctx, e.d.App, "acme", func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE org_billing SET status = 'exempt' WHERE tenant_id = 'acme'")
		return err
	})
	assert.Error(t, err, "the application role can't write org_billing")
	err = postgres.InTenantID(ctx, e.d.App, "acme", func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, "SELECT rport_operator_set_billing('acme', 'exempt', NULL, NULL, 'me', 'none', 'free', 'me')")
		return err
	})
	assert.Error(t, err, "the operator functions run only in the system tenant")
	// an organization sees only its own record
	_, err = e.op.SetPlan(ctx, op, "default", "team")
	require.NoError(t, err)
	var n int
	require.NoError(t, postgres.InTenantID(ctx, e.d.App, "acme", func(tx *sqlx.Tx) error {
		return tx.GetContext(ctx, &n, "SELECT count(*) FROM org_billing")
	}))
	assert.Equal(t, 1, n)
}

func TestSpendingLimitByAdministrators(t *testing.T) {
	e := newEnv(t, "acme")
	_, err := e.store.SetSpendingLimit(ctx, "acme", op, nil)
	assert.ErrorIs(t, err, billing.ErrInvalid, "the free plan has no overage")
	_, err = e.op.SetPlan(ctx, op, "acme", "team")
	require.NoError(t, err)
	limit := int64(5000)
	a, err := e.store.SetSpendingLimit(ctx, "acme", billing.Actor{Username: "admin"}, &limit)
	require.NoError(t, err)
	assert.EqualValues(t, 5000, *a.SpendingLimitCents)
	assert.Equal(t, []string{"set_plan", "set_spending_limit"}, auditActions(t, e, "acme"))
	a, err = e.store.SetSpendingLimit(ctx, "acme", billing.Actor{Username: "admin"}, nil)
	require.NoError(t, err)
	assert.Nil(t, a.SpendingLimitCents)
}

func TestUpkeepEnforcesCapsAndNotifiesOnce(t *testing.T) {
	e := newEnv(t, "free1", "team1")
	e.admin(t, "free1", "alice@example.com")
	e.admin(t, "free1", "bob")
	require.NoError(t, e.up.Run(ctx))
	e.startedAt(t, "free1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	e.startedAt(t, "team1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	_, err := e.op.SetPlan(ctx, op, "team1", "team")
	require.NoError(t, err)
	_, err = e.op.SetPlan(ctx, op, "free1", "free")
	require.NoError(t, err)
	hour := e.clock.Truncate(time.Hour)

	e.usage(t, "free1", hour.Add(-time.Hour), "tunnel_bytes_to_user", 9*billing.GiB)
	e.usage(t, "team1", hour.Add(-time.Hour), "upload_bytes_to_devices", 10*billing.GiB)
	require.NoError(t, e.up.Run(ctx))
	a, err := e.store.Account(ctx, "free1")
	require.NoError(t, err)
	assert.Equal(t, billing.TransferSoft, a.TransferState)
	assert.True(t, a.SoftCapReached)
	assert.EqualValues(t, 9*billing.GiB, a.Policy.UsedBytes)
	assert.EqualValues(t, 10*billing.GiB, a.Policy.AllowanceBytes)
	assert.Equal(t, 10, *maxClients(t, e, "free1"), "the free plan's device limit")
	assert.Equal(t, []string{"Transfer: most of this month's allowance is used"}, e.sent.subjects("free1"))
	assert.Equal(t, []string{"alice@example.com"}, e.sent.list[0].to)

	e.usage(t, "free1", hour.Add(-2*time.Hour), "tunnel_bytes_to_device", 2*billing.GiB)
	require.NoError(t, e.up.Run(ctx))
	require.NoError(t, e.up.Run(ctx))
	a, err = e.store.Account(ctx, "free1")
	require.NoError(t, err)
	assert.Equal(t, billing.TransferCapped, a.TransferState)
	assert.Len(t, e.sent.subjects("free1"), 2, "one notification per kind per period")

	pols, err := PolicySource{DB: e.d.Device}.TransferPolicies(ctx)
	require.NoError(t, err, "the device role reads the policies")
	by := map[string]billing.TenantPolicy{}
	for _, p := range pols {
		by[p.Organization] = p
	}
	assert.Equal(t, billing.TransferCapped, by["free1"].State)
	assert.EqualValues(t, 32_000, by["free1"].ThrottleRate)
	assert.EqualValues(t, 250_000, by["free1"].TunnelRate)
	assert.Equal(t, billing.TransferNormal, by["team1"].State, "Team bills overage: never capped without a spending limit")

	// metering stalls: the cap is not applied (fail open)
	e.clock = e.clock.Add(3 * time.Hour)
	require.NoError(t, e.up.Run(ctx))
	a, err = e.store.Account(ctx, "free1")
	require.NoError(t, err)
	assert.Equal(t, billing.TransferOver, a.TransferState)
	assert.Contains(t, a.Policy.FailOpen, "stale")

	// an operator lifts the cap
	e.usage(t, "free1", e.clock.Truncate(time.Hour).Add(-time.Hour), "tunnel_bytes_to_user", 1)
	until := e.clock.Add(24 * time.Hour)
	_, err = e.op.SetOverride(ctx, op, "free1", billing.Override{CapLiftedUntil: &until, ExtraTransfer: "5GiB"})
	require.NoError(t, err)
	require.NoError(t, e.up.Run(ctx))
	a, err = e.store.Account(ctx, "free1")
	require.NoError(t, err)
	assert.Equal(t, billing.TransferNormal, a.TransferState, "15 GiB allowance now, 11 GiB used: below the soft cap")
	assert.EqualValues(t, 15*billing.GiB, a.Policy.AllowanceBytes)
}

func TestUpkeepKeepsAnOperatorsDeviceLimit(t *testing.T) {
	e := newEnv(t, "acme")
	require.NoError(t, e.up.Run(ctx))
	_, err := e.op.SetPlan(ctx, op, "acme", "free")
	require.NoError(t, err)
	require.NoError(t, e.up.Run(ctx))
	assert.Equal(t, 10, *maxClients(t, e, "acme"))
	_, err = e.d.Owner.Exec("UPDATE tenants SET max_clients = 3 WHERE id = 'acme'")
	require.NoError(t, err)
	require.NoError(t, e.up.Run(ctx))
	assert.Equal(t, 3, *maxClients(t, e, "acme"), "a limit set by hand stays")
}

func TestUpkeepEndsTrials(t *testing.T) {
	e := newEnv(t, "card", "nocard")
	ends := e.clock.Add(2 * 24 * time.Hour)
	for _, id := range []string{"card", "nocard"} {
		_, err := e.op.SetPlan(ctx, op, id, "team")
		require.NoError(t, err)
		_, err = e.op.SetStatus(ctx, op, id, billing.StatusChange{Status: billing.StatusTrial, TrialEndsAt: &ends})
		require.NoError(t, err)
		e.admin(t, tenant.ID(id), id+"@example.com")
	}
	_, err := e.d.Owner.Exec("UPDATE org_billing SET has_payment_method = true WHERE tenant_id = 'card'")
	require.NoError(t, err)
	require.NoError(t, e.up.Run(ctx))
	assert.Equal(t, 25, *maxClients(t, e, "nocard"), "a trial without a card is capped at the trial's devices")
	assert.Nil(t, maxClients(t, e, "card"))
	a, err := e.store.Account(ctx, "nocard")
	require.NoError(t, err)
	assert.EqualValues(t, 25*billing.GiB, a.Policy.AllowanceBytes)
	assert.Positive(t, a.Policy.HardCapBytes)
	assert.Equal(t, []string{"Your trial ends in 3 days"}, e.sent.subjects("nocard"))

	e.clock = ends.Add(time.Minute)
	require.NoError(t, e.up.Run(ctx))
	a, err = e.store.Account(ctx, "nocard")
	require.NoError(t, err)
	assert.Equal(t, billing.StatusPaying, a.Status)
	assert.Equal(t, "free", a.Plan, "no card: the free plan")
	assert.Equal(t, 10, *maxClients(t, e, "nocard"))
	a, err = e.store.Account(ctx, "card")
	require.NoError(t, err)
	assert.Equal(t, billing.StatusPaying, a.Status)
	assert.Equal(t, "team", a.Plan)
	assert.Contains(t, auditActions(t, e, "nocard"), "trial_ended")
}

func TestUpkeepClosesPeriods(t *testing.T) {
	e := newEnv(t, "acme", "partner")
	require.NoError(t, e.up.Run(ctx))
	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	e.startedAt(t, "acme", time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC))
	e.startedAt(t, "partner", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	_, err := e.op.SetPlan(ctx, op, "acme", "team")
	require.NoError(t, err)
	_, err = e.op.SetStatus(ctx, op, "partner", billing.StatusChange{Status: billing.StatusExempt, Reason: "partner"})
	require.NoError(t, err)
	// both changes were made before October (the functions stamp them with the database's clock)
	_, err = e.d.Owner.Exec("UPDATE org_billing SET status_since = '2026-09-01', plan_since = '2026-09-01'")
	require.NoError(t, err)
	// October (15 days of billing for acme): 60 devices a day, 800 GiB
	e.devices(t, "acme", time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), 15, 60)
	e.usage(t, "acme", time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC), "tunnel_bytes_to_user", 800*billing.GiB)
	e.usage(t, "acme", time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC), "tunnel_bytes_to_user", 500*billing.GiB) // before billing started
	e.usage(t, "partner", time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC), "tunnel_bytes_to_user", 5*billing.GiB)

	// not before the settle delay
	e.clock = time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	require.NoError(t, e.up.Run(ctx))
	ps, err := e.store.Periods(ctx, "acme")
	require.NoError(t, err)
	assert.Empty(t, ps)

	e.clock = time.Date(2026, 11, 1, 2, 30, 0, 0, time.UTC)
	require.NoError(t, e.up.Run(ctx))
	ps, err = e.store.Periods(ctx, "acme")
	require.NoError(t, err)
	require.Len(t, ps, 1)
	p := ps[0]
	assert.Equal(t, oct, p.Start.UTC())
	assert.Equal(t, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), p.BilledFrom.UTC())
	assert.InDelta(t, 15.0/31, p.Proration, 1e-9)
	assert.EqualValues(t, 60, p.BillableDevices)
	assert.EqualValues(t, 10, p.ExtraDevices)
	assert.EqualValues(t, 800*billing.GiB, p.TransferBytes, "usage before billing started is not billed")
	assert.Positive(t, p.OverageGiB)
	assert.Equal(t, billing.PeriodHeld, p.State, "overage above $100 is held for review")
	assert.Equal(t, "team", p.Plan)

	// late usage: the closed period is frozen
	e.usage(t, "acme", time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC), "tunnel_bytes_to_user", billing.GiB)
	require.NoError(t, e.up.Run(ctx))
	ps, err = e.store.Periods(ctx, "acme")
	require.NoError(t, err)
	require.Len(t, ps, 1)
	assert.EqualValues(t, 800*billing.GiB, ps[0].TransferBytes)

	held, err := e.op.Periods(ctx, "", billing.PeriodHeld)
	require.NoError(t, err)
	require.Len(t, held, 1)
	require.NoError(t, e.op.ApprovePeriod(ctx, op, "acme", oct))
	assert.ErrorIs(t, e.op.ApprovePeriod(ctx, op, "acme", oct), billing.ErrNotFound)
	ps, err = e.op.Periods(ctx, "acme", "")
	require.NoError(t, err)
	assert.Equal(t, billing.PeriodReady, ps[0].State)
	assert.Equal(t, op.Username, *ps[0].ApprovedBy)

	pp, err := e.op.Periods(ctx, "partner", "")
	require.NoError(t, err)
	require.Len(t, pp, 10, "January to October")
	assert.Equal(t, billing.PeriodNeverCharged, pp[0].State)
	assert.EqualValues(t, 5*billing.GiB, pp[0].TransferBytes, "exempt organizations are metered")
}

// Turning billing on puts the organizations without a record on the free plan without limits, their device limit
// left alone, until an operator sets their plan.
func TestUpkeepLeavesUnassignedOrganizationsUnlimited(t *testing.T) {
	e := newEnv(t, "acme")
	require.NoError(t, e.up.Run(ctx))
	e.startedAt(t, "acme", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	e.usage(t, "acme", e.clock.Truncate(time.Hour).Add(-time.Hour), "tunnel_bytes_to_user", 5*billing.GiB)
	e.usage(t, "acme", e.clock.Truncate(time.Hour).Add(-2*time.Hour), "tunnel_bytes_to_user", 6*billing.GiB)
	require.NoError(t, e.up.Run(ctx))
	a, err := e.store.Account(ctx, "acme")
	require.NoError(t, err)
	assert.True(t, a.PlanUnassigned)
	assert.Equal(t, billing.TransferNormal, a.TransferState)
	assert.Nil(t, maxClients(t, e, "acme"), "the device limit is left alone")
	_, err = e.op.SetPlan(ctx, op, "acme", "free")
	require.NoError(t, err)
	require.NoError(t, e.up.Run(ctx))
	a, err = e.store.Account(ctx, "acme")
	require.NoError(t, err)
	assert.False(t, a.PlanUnassigned)
	assert.Equal(t, billing.TransferCapped, a.TransferState, "%+v", a.Policy)
}

// Just after midnight today's devices are not reported yet: the allowance comes from the days before.
func TestUpkeepAllowanceAtTheStartOfADay(t *testing.T) {
	e := newEnv(t, "acme")
	require.NoError(t, e.up.Run(ctx))
	e.startedAt(t, "acme", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	_, err := e.op.SetPlan(ctx, op, "acme", "team")
	require.NoError(t, err)
	e.devices(t, "acme", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), 23, 60)
	e.clock = time.Date(2026, 11, 24, 0, 10, 0, 0, time.UTC)
	require.NoError(t, e.up.Run(ctx))
	a, err := e.store.Account(ctx, "acme")
	require.NoError(t, err)
	assert.EqualValues(t, (250+60*5)*billing.GiB, a.Policy.AllowanceBytes, "60 devices, not 23×60/24")

	// the first day of a month: yesterday's devices
	e.devices(t, "acme", time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC), 7, 60)
	e.clock = time.Date(2026, 12, 1, 0, 10, 0, 0, time.UTC)
	require.NoError(t, e.up.Run(ctx))
	a, err = e.store.Account(ctx, "acme")
	require.NoError(t, err)
	assert.EqualValues(t, (250+60*5)*billing.GiB, a.Policy.AllowanceBytes)
}

// A period that failed open stays open until an operator clears it, even once the spike is in the average.
func TestUpkeepFailOpenHoldsForThePeriod(t *testing.T) {
	e := newEnv(t, "acme")
	require.NoError(t, e.up.Run(ctx))
	e.startedAt(t, "acme", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	_, err := e.op.SetPlan(ctx, op, "acme", "free")
	require.NoError(t, err)
	hour := e.clock.Truncate(time.Hour)
	e.usage(t, "acme", hour.Add(-time.Hour), "tunnel_bytes_to_user", 400*billing.GiB) // a counting bug
	require.NoError(t, e.up.Run(ctx))
	a, err := e.store.Account(ctx, "acme")
	require.NoError(t, err)
	assert.Equal(t, billing.TransferOver, a.TransferState)
	assert.Contains(t, a.Policy.FailOpen, "implausible")

	// an hour later the spike is in the average, the last hour is quiet
	e.clock = e.clock.Add(time.Hour)
	e.usage(t, "acme", hour, "tunnel_bytes_to_user", 1)
	require.NoError(t, e.up.Run(ctx))
	a, err = e.store.Account(ctx, "acme")
	require.NoError(t, err)
	assert.Equal(t, billing.TransferOver, a.TransferState, "still open")

	_, err = e.op.SetOverride(ctx, op, "acme", billing.Override{ClearFailOpen: true})
	require.NoError(t, err)
	require.NoError(t, e.up.Run(ctx))
	a, err = e.store.Account(ctx, "acme")
	require.NoError(t, err)
	assert.Equal(t, billing.TransferCapped, a.TransferState, "an operator checked it")
}

func TestOverrideKeepsWhatIsNotGiven(t *testing.T) {
	e := newEnv(t, "acme")
	until := e.clock.Add(48 * time.Hour)
	_, err := e.op.SetOverride(ctx, op, "acme", billing.Override{CapLiftedUntil: &until})
	require.NoError(t, err)
	a, err := e.op.SetOverride(ctx, op, "acme", billing.Override{ExtraTransfer: "5GiB"})
	require.NoError(t, err)
	require.NotNil(t, a.CapLiftedUntil, "the lift stays")
	assert.EqualValues(t, 5*billing.GiB, a.ExtraTransferBytes)
	a, err = e.op.SetOverride(ctx, op, "acme", billing.Override{EndCapLift: true})
	require.NoError(t, err)
	assert.Nil(t, a.CapLiftedUntil)
	assert.EqualValues(t, 5*billing.GiB, a.ExtraTransferBytes, "the extra transfer stays")
}

// Status changes: ending a trial early charges from now; a change made after a month ended is not applied to it.
func TestStatusChangesAndPeriods(t *testing.T) {
	e := newEnv(t, "acme", "beta")
	e.clock = time.Now().UTC()
	ends := e.clock.Add(10 * 24 * time.Hour)
	_, err := e.op.SetStatus(ctx, op, "acme", billing.StatusChange{Status: billing.StatusTrial, TrialEndsAt: &ends})
	require.NoError(t, err)
	a, err := e.op.SetStatus(ctx, op, "acme", billing.StatusChange{Status: billing.StatusPaying})
	require.NoError(t, err)
	require.NotNil(t, a.TrialEndsAt)
	assert.False(t, a.TrialEndsAt.After(time.Now()), "the trial ended now")
	require.NotNil(t, a.PreviousStatus)
	assert.Equal(t, billing.StatusTrial, *a.PreviousStatus)

	org, err := e.store.Account(ctx, "acme")
	require.NoError(t, err)
	assert.Nil(t, org.PreviousStatus, "the organization's view leaves out the operators' fields")
	assert.Empty(t, org.UpdatedBy)

	// beta, paying on Team all October, comped from November 1 01:00, before October is closed
	_, err = e.op.SetPlan(ctx, op, "beta", "team")
	require.NoError(t, err)
	until := time.Now().AddDate(0, 3, 0)
	_, err = e.op.SetStatus(ctx, op, "beta", billing.StatusChange{Status: billing.StatusComped, CompedUntil: &until})
	require.NoError(t, err)
	_, err = e.d.Owner.Exec("UPDATE org_billing SET started_at = '2026-01-01', plan_since = '2026-09-01', status_since = '2026-11-01 01:00Z' WHERE tenant_id = 'beta'")
	require.NoError(t, err)
	e.clock = time.Date(2026, 11, 1, 2, 30, 0, 0, time.UTC)
	require.NoError(t, e.up.Run(ctx))
	ps, err := e.op.Periods(ctx, "beta", "")
	require.NoError(t, err)
	require.NotEmpty(t, ps)
	assert.Equal(t, billing.StatusPaying, ps[0].Status, "October was paying")
	assert.Equal(t, billing.PeriodReady, ps[0].State)
	assert.EqualValues(t, 4900, ps[0].TotalCents)
}
