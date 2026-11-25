package billingprovider

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/billing"
	"github.com/openrport/openrport/server/billing/stripe"
	"github.com/openrport/openrport/server/billing/stripe/stripetest"
	"github.com/openrport/openrport/server/repotest"
	pgbilling "github.com/openrport/openrport/server/storage/postgres/billing"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
)

const plansFile = `
free_plan = "free"
[[plan]]
  id = "free"
  overage = "cap"
  transfer_base = "10GiB"
[[plan]]
  id = "starter"
  base_price = 10.0
  included_devices = 10
  price_per_device = 1.0
  transfer_base = "25GiB"
  overage = "bill"
  overage_price_per_gib = 0.25
  provider_prices = { base = "price_base", devices = "price_devices", overage = "price_overage" }
`

func insertPeriod(t *testing.T, d *pgtest.DB, org, status, state string, start time.Time, extra, gib int64) {
	t.Helper()
	tx, err := d.Owner.Beginx()
	require.NoError(t, err)
	_, err = tx.Exec("SELECT set_config('rport.billing_all', 'on', true)")
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO billing_periods (tenant_id, period_start, period_end, billed_from, proration, plan, billing_status,
		billable_devices, peak_devices, device_days, extra_devices, tunnel_bytes_to_user, tunnel_bytes_to_device,
		upload_bytes_to_devices, transfer_bytes, allowance_bytes, overage_bytes, overage_gib, base_cents, device_cents,
		overage_cents, total_cents, state)
		VALUES ($1, $2::date, $3::date, $2::date, 1, 'starter', $4, $5::bigint, $5::bigint, 0, $6::bigint, 0, 0, 0, 0, 0, $7::bigint * 1073741824, $7::bigint, 1000, $6::bigint * 100, $7::bigint * 25, 1000, $8)`,
		org, start.Format("2006-01-02"), start.AddDate(0, 1, 0).Format("2006-01-02"), status, extra+10, extra, gib, state)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

func TestReportingFromClosedPeriodsEndToEnd(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	a, b := string(repotest.TenantA), string(repotest.TenantB)
	plans, err := billing.ParsePlans([]byte(plansFile))
	require.NoError(t, err)

	f := stripetest.New(t)
	f.AddLicensedPrice("price_base")
	f.AddMeteredPrice("price_devices", "procy_extra_devices")
	f.AddMeteredPrice("price_overage", "procy_overage_gib")
	p, err := stripe.New(stripe.Options{SecretKey: stripetest.SecretKey, WebhookSecret: stripetest.WebhookSecret, BackendURL: f.Server.URL})
	require.NoError(t, err)

	pgStore := pgbilling.Store{DB: d.App, Plans: plans}
	store := Store{DB: d.App}
	sync := Sync{DB: d.App, FreePlan: "free", By: "billing:stripe"}
	svc, err := billing.NewService(billing.Options{
		Provider: p, Store: store, Plans: plans, PlanIDs: []string{"free", "starter"}, FreePlan: "free",
		Records: Records{Store: pgStore}, Periods: Periods{Operator: &pgbilling.Operator{DB: d.App, Plans: plans}}, Sync: sync,
	})
	require.NoError(t, err)

	// both organizations have a customer; b is exempt
	require.NoError(t, store.PutAccount(ctx, &billing.ProviderAccount{Organization: a, Provider: "stripe", CustomerID: "cus_a"}))
	require.NoError(t, store.PutAccount(ctx, &billing.ProviderAccount{Organization: b, Provider: "stripe", CustomerID: "cus_b"}))
	require.NoError(t, sync.ProviderState(ctx, a, "cus_a", "sub_a", true, "starter"))
	acc, err := pgStore.Account(ctx, repotest.TenantA)
	require.NoError(t, err)
	assert.Equal(t, "starter", acc.Plan)
	assert.Equal(t, "cus_a", acc.ProviderCustomerID)
	assert.Equal(t, "sub_a", acc.ProviderSubscriptionID)
	assert.True(t, acc.HasPaymentMethod)

	start := billing.MonthStart(time.Now().UTC()).AddDate(0, -1, 0)
	insertPeriod(t, d, a, "paying", "ready", start, 12, 21)
	insertPeriod(t, d, b, "exempt", "never_charged", start, 300, 900)
	insertPeriod(t, d, a, "paying", "held", start.AddDate(0, -1, 0), 5, 5000)

	require.NoError(t, svc.ReportClosedPeriods(ctx))
	ev := f.Events()
	require.Len(t, ev, 2)
	for _, e := range ev {
		assert.Equal(t, "cus_a", e.Customer, "the exempt organization is never sent")
	}
	periods, err := pgStore.Periods(ctx, repotest.TenantA)
	require.NoError(t, err)
	states := map[string]billing.PeriodState{}
	for _, p := range periods {
		states[p.Start.Format("2006-01")] = p.State
	}
	assert.Equal(t, billing.PeriodReported, states[start.Format("2006-01")])
	assert.Equal(t, billing.PeriodHeld, states[start.AddDate(0, -1, 0).Format("2006-01")], "held overage waits for an operator")

	reports, err := store.Reports(ctx, start)
	require.NoError(t, err)
	require.Len(t, reports, 2)

	// nothing more on the next run
	require.NoError(t, svc.ReportClosedPeriods(ctx))
	assert.Len(t, f.Events(), 2)
}

func TestProviderStateNeverChangesAnExemptPlan(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	a := string(repotest.TenantA)
	plans, err := billing.ParsePlans([]byte(plansFile))
	require.NoError(t, err)
	op := &pgbilling.Operator{DB: d.App, Plans: plans}
	sysCtx := ctx
	_, err = op.SetStatus(sysCtx, billing.Actor{Username: "op"}, a, billing.StatusChange{Status: billing.StatusExempt, Reason: "partner"})
	require.NoError(t, err)
	sync := Sync{DB: d.App, FreePlan: "free", By: "billing:stripe"}
	require.NoError(t, sync.ProviderState(ctx, a, "cus_a", "", false, "starter"))
	acc, err := pgbilling.Store{DB: d.App, Plans: plans}.Account(ctx, repotest.TenantA)
	require.NoError(t, err)
	assert.Equal(t, billing.StatusExempt, acc.Status)
	assert.Equal(t, "free", acc.Plan)
	// unknown or reserved organizations change nothing
	require.NoError(t, sync.ProviderState(ctx, "_system", "cus_s", "", false, "starter"))
}
