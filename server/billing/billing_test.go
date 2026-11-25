package billing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/share/tenant"
)

// examplePlans are the design's sketch (PLACEHOLDER numbers).
const examplePlans = `
free_plan = "free"
[trial]
  plan = "team"
  days = 14
  max_clients = 25
  transfer = "25GiB"

[[plan]]
  id = "free"
  name = "Free"
  included_devices = 5
  max_clients = 10
  price_per_device = 0
  transfer_base = "10GiB"
  transfer_per_device = "0"
  overage = "cap"
  tunnel_rate_limit = "2Mbit"
  throttle_rate = "256kbit"

[[plan]]
  id = "starter"
  name = "Starter"
  base_price = 10.00
  included_devices = 10
  price_per_device = 1.00
  transfer_base = "25GiB"
  transfer_per_device = "2GiB"
  overage = "bill"
  overage_price_per_gib = 0.25
  tunnel_rate_limit = "20Mbit"
  provider_prices = { base = "price_x", devices = "price_y", overage = "price_z" }

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
  annual_discount_months = 2
`

func plans(t *testing.T) *Plans {
	t.Helper()
	p, err := ParsePlans([]byte(examplePlans))
	require.NoError(t, err)
	return p
}

func day(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestParsePlans(t *testing.T) {
	p := plans(t)
	free := p.FreePlan()
	require.NotNil(t, free)
	assert.Equal(t, ByteSize(10*GiB), free.TransferBase)
	assert.Equal(t, RateLimit(250_000), free.TunnelRateLimit)
	assert.Equal(t, RateLimit(32_000), free.ThrottleRate)
	assert.Equal(t, 100, free.HardCapPercent, "cap plans default to a hard cap at 100%")
	assert.Equal(t, 80, free.SoftCapPercent)
	st := p.Get("starter")
	assert.Equal(t, 0, st.HardCapPercent, "bill plans have no hard cap by default")
	assert.Equal(t, DefaultThrottleRate, st.ThrottleRate)
	assert.Equal(t, "price_y", st.ProviderPrices["devices"])
	assert.Equal(t, 25*GiB+22*2*GiB, st.Allowance(22))
	assert.Equal(t, "team", p.Trial.Plan)
	assert.Equal(t, 25, *p.Trial.MaxClients)
	assert.Nil(t, p.Get("nope"))
}

func TestParsePlansRefusesInvalid(t *testing.T) {
	base := "[[plan]]\nid = \"free\"\noverage = \"cap\"\n"
	for name, in := range map[string]string{
		"empty":           "",
		"unknown key":     base + "color = \"red\"\n",
		"duplicate":       base + base,
		"bad id":          "[[plan]]\nid = \"Free!\"\n",
		"no free plan":    "free_plan = \"x\"\n" + base,
		"priced free":     "[[plan]]\nid = \"free\"\nbase_price = 1.0\n",
		"bill free":       "[[plan]]\nid = \"free\"\noverage = \"bill\"\noverage_price_per_gib = 1.0\n",
		"bad overage":     base + "[[plan]]\nid = \"p\"\noverage = \"maybe\"\n",
		"bill no price":   base + "[[plan]]\nid = \"p\"\noverage = \"bill\"\n",
		"negative price":  base + "[[plan]]\nid = \"p\"\nbase_price = -1.0\n",
		"bad size":        base + "[[plan]]\nid = \"p\"\ntransfer_base = \"10 parsecs\"\n",
		"bad rate":        base + "[[plan]]\nid = \"p\"\ntunnel_rate_limit = \"2MB\"\n",
		"soft cap 0..100": base + "[[plan]]\nid = \"p\"\nsoft_cap = 120\n",
		"hard below soft": base + "[[plan]]\nid = \"p\"\nsoft_cap = 80\nhard_cap = 50\n",
		"trial plan":      base + "[trial]\nplan = \"gold\"\n",
		"trial days":      base + "[trial]\ndays = 400\n",
	} {
		_, err := ParsePlans([]byte(in))
		assert.ErrorIs(t, err, ErrInvalid, name)
	}
}

func TestSizesAndRates(t *testing.T) {
	for in, want := range map[string]ByteSize{"0": 0, "10GiB": ByteSize(10 * GiB), "1.5 KiB": 1536, "2GB": 2e9, "7": 7, "1TiB": 1 << 40} {
		got, err := ParseByteSize(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	assert.Equal(t, "10GiB", ByteSize(10*GiB).String())
	for in, want := range map[string]RateLimit{"0": 0, "2Mbit": 250_000, "256kbit": 32_000, "1Gbit": 125_000_000, "8bit": 1} {
		got, err := ParseRateLimit(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	assert.Equal(t, "2Mbit", RateLimit(250_000).String())
	_, err := ParseRateLimit("5")
	assert.Error(t, err, "a rate needs a unit")
}

func TestStatusChangeValidate(t *testing.T) {
	now := day("2026-11-24")
	later := now.Add(14 * 24 * time.Hour)
	ok := []StatusChange{
		{Status: StatusPaying},
		{Status: StatusTrial, TrialEndsAt: &later},
		{Status: StatusComped, CompedUntil: &later},
		{Status: StatusExempt, Reason: "partner"},
		{Status: StatusExempt, Reason: "internal", ExemptLimits: ExemptLimitsNone},
	}
	for _, c := range ok {
		assert.NoError(t, c.Validate(now), c.Status)
	}
	c := StatusChange{Status: StatusExempt, Reason: "partner"}
	require.NoError(t, c.Validate(now))
	assert.Equal(t, ExemptLimitsPlan, c.ExemptLimits)
	past := now.Add(-48 * time.Hour)
	bad := []StatusChange{
		{Status: "free"},
		{Status: StatusTrial},
		{Status: StatusTrial, TrialEndsAt: &past},
		{Status: StatusComped},
		{Status: StatusComped, CompedUntil: &past},
		{Status: StatusExempt},
		{Status: StatusPaying, TrialEndsAt: &later},
		{Status: StatusPaying, ExemptLimits: ExemptLimitsNone},
		{Status: StatusExempt, Reason: "x", ExemptLimits: "some"},
	}
	for _, c := range bad {
		assert.ErrorIs(t, c.Validate(now), ErrInvalid, "%+v", c)
	}
}

func devices(n int64, days int) []int64 {
	out := make([]int64, days)
	for i := range out {
		out[i] = n
	}
	return out
}

// Worked example 1: Starter, growing fleet; 21.3 average → 22 billable, 90 GiB used.
func TestClosePeriodStarterGrowingFleet(t *testing.T) {
	p := plans(t)
	b := &Billing{Organization: "acme", Plan: "starter", Status: StatusPaying, StartedAt: day("2026-01-01")}
	// 30 days: device-days 639 → 21.3 average
	daily := devices(21, 30)
	for i := 0; i < 9; i++ {
		daily[i] = 22
	}
	u := PeriodUsage{DailyDevices: daily, TunnelBytesToUser: 80 * GiB, TunnelBytesToDevice: 6 * GiB, UploadBytesToDevices: 4 * GiB}
	got := ClosePeriod(b, p.Get("starter"), day("2026-11-01"), u, 0, day("2026-12-01").Add(3*time.Hour))
	assert.Equal(t, day("2026-11-01"), got.BilledFrom)
	assert.Equal(t, day("2026-12-01"), got.End)
	assert.Equal(t, 1.0, got.Proration)
	assert.EqualValues(t, 22, got.BillableDevices)
	assert.EqualValues(t, 22, got.PeakDevices)
	assert.EqualValues(t, 12, got.ExtraDevices)
	assert.EqualValues(t, 69*GiB, got.AllowanceBytes)
	assert.EqualValues(t, 90*GiB, got.TransferBytes)
	assert.EqualValues(t, 21, got.OverageGiB)
	assert.EqualValues(t, 1000, got.BaseCents)
	assert.EqualValues(t, 1200, got.DeviceCents)
	assert.EqualValues(t, 525, got.OverageCents)
	assert.EqualValues(t, 2725, got.TotalCents)
	assert.Equal(t, PeriodReady, got.State)

	held := ClosePeriod(b, p.Get("starter"), day("2026-11-01"), u, 500, time.Now())
	assert.Equal(t, PeriodHeld, held.State, "overage above the review amount is held")
}

func TestClosePeriodProratesTheFirstMonth(t *testing.T) {
	p := plans(t)
	b := &Billing{Organization: "acme", Plan: "starter", Status: StatusPaying, StartedAt: day("2026-11-21").Add(15 * time.Hour)}
	got := ClosePeriod(b, p.Get("starter"), day("2026-11-01"), PeriodUsage{DailyDevices: devices(30, 10)}, 0, time.Now())
	assert.Equal(t, day("2026-11-21"), got.BilledFrom)
	assert.InDelta(t, 10.0/30, got.Proration, 1e-9)
	assert.EqualValues(t, 30, got.BillableDevices, "the average over the days billed")
	assert.EqualValues(t, 333, got.BaseCents)
	assert.EqualValues(t, 667, got.DeviceCents, "20 extra × $1 × 10/30")
	assert.InDelta(t, float64(25*GiB+30*2*GiB)/3, float64(got.AllowanceBytes), 2)
}

func TestClosePeriodStatuses(t *testing.T) {
	p := plans(t)
	nov := day("2026-11-01")
	team := p.Get("team")
	u := PeriodUsage{DailyDevices: devices(60, 30), TunnelBytesToUser: 2 << 40}

	exempt := &Billing{Organization: "partner", Plan: "team", Status: StatusExempt, StartedAt: day("2026-01-01")}
	got := ClosePeriod(exempt, team, nov, u, 0, time.Now())
	assert.Equal(t, PeriodNeverCharged, got.State)
	assert.Positive(t, got.TotalCents, "the list price is shown")

	until := day("2027-03-31")
	comped := &Billing{Plan: "team", Status: StatusComped, CompedUntil: &until, StartedAt: day("2026-01-01")}
	assert.Equal(t, PeriodNotCharged, ClosePeriod(comped, team, day("2027-03-01"), u, 0, time.Now()).State)
	// after the comped date: April is billed normally (the upkeep made it paying)
	comped.Status = StatusPaying
	apr := ClosePeriod(comped, team, day("2027-04-01"), PeriodUsage{DailyDevices: devices(60, 30)}, 0, time.Now())
	assert.Equal(t, PeriodReady, apr.State)
	assert.Equal(t, 1.0, apr.Proration)
	assert.EqualValues(t, 4900+800, apr.TotalCents, "worked example 5: $49 + 10 × $0.80")

	mid := day("2026-11-16")
	trial := &Billing{Plan: "team", Status: StatusTrial, TrialEndsAt: &mid, StartedAt: day("2026-11-02")}
	assert.Equal(t, PeriodNotCharged, ClosePeriod(trial, team, nov, u, 0, time.Now()).State)
	trial.Status = StatusPaying // the trial ended with a card on file
	got = ClosePeriod(trial, team, nov, PeriodUsage{DailyDevices: devices(50, 15)}, 0, time.Now())
	assert.Equal(t, mid, got.BilledFrom)
	assert.InDelta(t, 0.5, got.Proration, 1e-9)
	assert.EqualValues(t, 2450, got.TotalCents)

	free := &Billing{Plan: "free", Status: StatusPaying, StartedAt: day("2026-01-01")}
	got = ClosePeriod(free, p.FreePlan(), nov, PeriodUsage{DailyDevices: devices(5, 30), TunnelBytesToUser: 50 * GiB}, 0, time.Now())
	assert.Equal(t, PeriodNotCharged, got.State)
	assert.Zero(t, got.OverageBytes, "capped plans have no overage")
}

func usage(used int64) CurrentUsage {
	return CurrentUsage{TransferBytes: used, DeviceDays: 5 * 10, Days: 10, NewestUsage: day("2026-11-24").Add(-30 * time.Minute)}
}

func TestEvaluate(t *testing.T) {
	p := plans(t)
	now := day("2026-11-24")
	free := &Billing{Organization: "f", Plan: "free", Status: StatusPaying, StartedAt: day("2026-01-01")}

	pol := Evaluate(free, p, usage(GiB), EnforceOptions{}, now)
	assert.Equal(t, TransferNormal, pol.State)
	assert.EqualValues(t, 10*GiB, pol.AllowanceBytes)
	assert.EqualValues(t, 8*GiB, pol.SoftCapBytes)
	assert.EqualValues(t, 10*GiB, pol.HardCapBytes)
	assert.Equal(t, RateLimit(250_000), pol.TunnelRate)
	assert.Equal(t, 10, *pol.MaxClients)
	assert.Equal(t, TransferSoft, Evaluate(free, p, usage(9*GiB), EnforceOptions{}, now).State)
	// worked example 2: a big download reaches the cap
	pol = Evaluate(free, p, usage(10*GiB), EnforceOptions{}, now)
	assert.Equal(t, TransferCapped, pol.State)
	assert.Empty(t, pol.FailOpen)

	// fail open: stale metering, implausible growth
	stale := usage(11 * GiB)
	stale.NewestUsage = now.Add(-3 * time.Hour)
	pol = Evaluate(free, p, stale, EnforceOptions{}, now)
	assert.Equal(t, TransferOver, pol.State)
	assert.Contains(t, pol.FailOpen, "stale")
	spike := usage(500 * GiB)
	spike.LastHourBytes, spike.AvgHourBytes = 490*GiB, float64(GiB)
	pol = Evaluate(free, p, spike, EnforceOptions{}, now)
	assert.Equal(t, TransferOver, pol.State)
	assert.Contains(t, pol.FailOpen, "implausible")

	// an operator lifted the cap
	lift := now.Add(time.Hour)
	lifted := *free
	lifted.CapLiftedUntil = &lift
	assert.Equal(t, TransferOver, Evaluate(&lifted, p, usage(20*GiB), EnforceOptions{}, now).State)

	// bill plans: over, never capped without a spending limit
	team := &Billing{Organization: "t", Plan: "team", Status: StatusPaying, StartedAt: day("2026-01-01")}
	tu := CurrentUsage{TransferBytes: 900 * GiB, DeviceDays: 60 * 10, Days: 10, NewestUsage: now}
	pol = Evaluate(team, p, tu, EnforceOptions{}, now)
	assert.Equal(t, TransferOver, pol.State)
	assert.EqualValues(t, 550*GiB, pol.AllowanceBytes)
	assert.Zero(t, pol.HardCapBytes)
	assert.Zero(t, pol.TunnelRate)
	// worked example 5: a $50 spending limit caps at 800 GiB
	limit := int64(5000)
	team.SpendingLimitCents = &limit
	tu.TransferBytes = 799 * GiB
	pol = Evaluate(team, p, tu, EnforceOptions{}, now)
	assert.Equal(t, TransferOver, pol.State)
	assert.EqualValues(t, 800*GiB, pol.HardCapBytes)
	tu.TransferBytes = 800 * GiB
	assert.Equal(t, TransferCapped, Evaluate(team, p, tu, EnforceOptions{}, now).State)

	// a trial without a card: Team's rates, the trial's ceilings, capped
	ends := now.Add(7 * 24 * time.Hour)
	trial := &Billing{Organization: "tr", Plan: "team", Status: StatusTrial, TrialEndsAt: &ends, StartedAt: day("2026-11-01")}
	pol = Evaluate(trial, p, usage(25*GiB), EnforceOptions{}, now)
	assert.Equal(t, TransferCapped, pol.State)
	assert.EqualValues(t, 25*GiB, pol.AllowanceBytes)
	assert.Equal(t, 25, *pol.MaxClients)
	trial.HasPaymentMethod = true
	pol = Evaluate(trial, p, usage(25*GiB), EnforceOptions{}, now)
	assert.Equal(t, TransferNormal, pol.State)
	assert.Nil(t, pol.MaxClients)

	// exempt without limits
	ex := &Billing{Organization: "x", Plan: "free", Status: StatusExempt, ExemptLimits: ExemptLimitsNone}
	pol = Evaluate(ex, p, usage(1<<40), EnforceOptions{}, now)
	assert.Equal(t, TransferNormal, pol.State)
	assert.Zero(t, pol.TunnelRate)

	// unknown plan (removed from the file): the free plan's limits
	gone := &Billing{Organization: "g", Plan: "gold", Status: StatusPaying}
	assert.Equal(t, "free", Evaluate(gone, p, usage(0), EnforceOptions{}, now).Plan)
}

type fakeSource struct {
	rows []TenantPolicy
	err  error
}

func (f *fakeSource) TransferPolicies(context.Context) ([]TenantPolicy, error) { return f.rows, f.err }

func TestEnforcerFailsOpen(t *testing.T) {
	p := plans(t)
	now := time.Now()
	src := &fakeSource{rows: []TenantPolicy{
		{Organization: "a", Plan: "free", State: TransferCapped, TunnelRate: 250_000, ThrottleRate: 32_000, UpdatedAt: now},
		{Organization: "b", Plan: "starter", State: TransferOver, TunnelRate: 2_500_000, ThrottleRate: 32_000, UpdatedAt: now},
		{Organization: "c", Plan: "free", State: TransferCapped, TunnelRate: 250_000, ThrottleRate: 32_000, UpdatedAt: now.Add(-2 * time.Hour)},
	}}
	e := NewEnforcer(src, p, nil)
	clock := now
	e.now = func() time.Time { return clock }
	require.NoError(t, e.Refresh(context.Background()))

	err := e.Allow("a")
	require.ErrorIs(t, err, ErrTransferLimit)
	assert.Contains(t, err.Error(), `"Free"`)
	assert.EqualValues(t, 32_000, e.TunnelRate("a"), "throttled")
	assert.NoError(t, e.Allow("b"))
	assert.EqualValues(t, 2_500_000, e.TunnelRate("b"))
	assert.NoError(t, e.Allow("c"), "a cap the upkeep wrote long ago is not applied")
	assert.NoError(t, e.Allow(tenant.ID("unknown")))
	assert.Zero(t, e.TunnelRate("unknown"))

	// the source fails: the last policies are kept for an hour, then only the per-tunnel rates apply
	src.err = errors.New("down")
	assert.Error(t, e.Refresh(context.Background()))
	clock = now.Add(30 * time.Minute)
	src.rows[0].UpdatedAt = clock
	assert.Error(t, e.Allow("a"))
	clock = now.Add(61 * time.Minute)
	assert.NoError(t, e.Allow("a"))
	assert.EqualValues(t, 250_000, e.TunnelRate("a"))

	var nilE *Enforcer
	assert.NoError(t, nilE.Allow("a"))
	assert.Zero(t, nilE.TunnelRate("a"))
	assert.Nil(t, nilE.RateFunc("a"))
}

func TestAsOfAndStatusHistory(t *testing.T) {
	p := plans(t)
	team := p.Get("team")
	exempt, trial := StatusExempt, StatusTrial
	since := day("2026-11-21")
	b := &Billing{Plan: "team", Status: StatusPaying, PreviousStatus: &exempt, StatusSince: &since, StartedAt: day("2026-01-01")}
	got := ClosePeriod(b, team, day("2026-11-01"), PeriodUsage{DailyDevices: devices(50, 10)}, 0, time.Now())
	assert.Equal(t, since, got.BilledFrom, "exempt until the 21st: billed from then")
	assert.InDelta(t, 10.0/30, got.Proration, 1e-9)

	// a change after the month ended doesn't apply to it
	after := day("2026-12-01").Add(time.Hour)
	free := "free"
	ends := day("2027-01-01")
	b = &Billing{Plan: "team", Status: StatusTrial, TrialEndsAt: &ends, PreviousStatus: &exempt, StatusSince: &after, PreviousPlan: &free, PlanSince: &after}
	at := b.AsOf(day("2026-12-01"))
	assert.Equal(t, StatusExempt, at.Status)
	assert.Equal(t, "free", at.Plan)
	assert.Nil(t, at.TrialEndsAt)
	at = b.AsOf(day("2027-01-01"))
	assert.Equal(t, StatusTrial, at.Status)
	_ = trial
}

func TestEvaluateUnassignedAndStickyFailOpen(t *testing.T) {
	p := plans(t)
	now := day("2026-11-24")
	b := &Billing{Organization: "u", Plan: "free", Status: StatusPaying, PlanUnassigned: true, StartedAt: day("2026-01-01")}
	pol := Evaluate(b, p, usage(1<<40), EnforceOptions{}, now)
	assert.Equal(t, TransferNormal, pol.State)
	assert.Nil(t, pol.MaxClients)
	assert.Zero(t, pol.TunnelRate)

	b.PlanUnassigned = false
	u := usage(20 * GiB)
	u.FailedOpen = "implausible transfer"
	pol = Evaluate(b, p, u, EnforceOptions{}, now)
	assert.Equal(t, TransferOver, pol.State)
	assert.Equal(t, "implausible transfer", pol.FailOpen)
}

// The plan catalogue has prices in cents and never the provider's price ids.
func TestCatalogue(t *testing.T) {
	c := plans(t).Catalogue()
	require.Len(t, c, 3)
	assert.Equal(t, "free", c[0].ID)
	assert.True(t, c[0].Free)
	assert.False(t, c[0].Trial)
	assert.EqualValues(t, 10*GiB, c[0].TransferBaseBytes)
	assert.Equal(t, OverageCap, c[0].Overage)
	assert.False(t, c[0].Purchasable)
	assert.Equal(t, 100, c[0].HardCapPercent)

	st := c[1]
	assert.EqualValues(t, 1000, st.MonthlyPriceCents)
	assert.EqualValues(t, 10, st.IncludedDevices)
	assert.EqualValues(t, 100, st.PricePerDeviceCents)
	assert.EqualValues(t, 2*GiB, st.TransferPerDeviceBytes)
	assert.Equal(t, OverageBill, st.Overage)
	assert.EqualValues(t, 25, st.OveragePricePerGiBCents)
	assert.True(t, st.Purchasable)

	assert.True(t, c[2].Trial)
	assert.Equal(t, 14, c[2].TrialDays)
	assert.EqualValues(t, 4900, c[2].MonthlyPriceCents)
	b, err := json.Marshal(c)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "price_x")
	assert.NotContains(t, string(b), "provider")
}
