package billingprovider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/billing"
	"github.com/openrport/openrport/server/repotest"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var ctx = context.Background()

func TestAccountsAndCustomerMapping(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	s := Store{DB: d.App}
	a, b := string(repotest.TenantA), string(repotest.TenantB)

	got, err := s.Account(ctx, a)
	require.NoError(t, err)
	assert.Nil(t, got)

	failed := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	acc := &billing.ProviderAccount{Organization: a, Provider: "stripe", CustomerID: "cus_a", BillingEmail: "x@a.test",
		SubscriptionID: "sub_a", SubscriptionStatus: billing.SubscriptionActive, PriceIDs: []string{"p1", "p2"},
		HasPaymentMethod: true, Dunning: billing.Dunning{State: billing.DunningPastDue, FailedAt: &failed, FailedInvoice: "in_1"}}
	require.NoError(t, s.PutAccount(ctx, acc))
	got, err = s.Account(ctx, a)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "cus_a", got.CustomerID)
	assert.Equal(t, []string{"p1", "p2"}, got.PriceIDs)
	assert.Equal(t, billing.DunningPastDue, got.State)
	assert.True(t, failed.Equal(*got.FailedAt))
	assert.True(t, got.HasPaymentMethod)

	org, err := s.OrganizationOf(ctx, "stripe", "cus_a")
	require.NoError(t, err)
	assert.Equal(t, a, org)
	org, err = s.OrganizationOf(ctx, "stripe", "cus_unknown")
	require.NoError(t, err)
	assert.Empty(t, org)

	// a customer is linked to one organization only
	err = s.PutAccount(ctx, &billing.ProviderAccount{Organization: b, Provider: "stripe", CustomerID: "cus_a"})
	assert.Error(t, err)
	require.NoError(t, s.PutAccount(ctx, &billing.ProviderAccount{Organization: b, Provider: "stripe", CustomerID: "cus_b"}))

	// reserved tenants are refused
	assert.Error(t, s.PutAccount(ctx, &billing.ProviderAccount{Organization: "_system", Provider: "stripe", CustomerID: "cus_s"}))

	all, err := s.Accounts(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 2)

	// row-level security: B can't see A's account through a tenant transaction
	var n int
	require.NoError(t, d.Owner.Get(&n, "SELECT count(*) FROM billing_provider_accounts"))
	assert.Equal(t, 2, n)
}

func TestAppRoleCannotWriteDirectly(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	_, err := d.App.Exec("INSERT INTO billing_events (provider, event_id, type) VALUES ('stripe', 'evt', 't')")
	assert.Error(t, err, "the application role writes only through the functions")
	_, err = d.App.Exec("DELETE FROM billing_usage_reports")
	assert.Error(t, err)
}

func TestApplyEventOnceAndRollsBackOnError(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	s := Store{DB: d.App}
	a := string(repotest.TenantA)
	require.NoError(t, s.PutAccount(ctx, &billing.ProviderAccount{Organization: a, Provider: "stripe", CustomerID: "cus_a"}))

	boom := errors.New("boom")
	applied, err := s.ApplyEvent(ctx, "stripe", "evt_1", "invoice.paid", a, func(cur *billing.ProviderAccount) (*billing.ProviderAccount, error) {
		return nil, boom
	})
	assert.ErrorIs(t, err, boom)
	assert.False(t, applied)

	calls := 0
	update := func(cur *billing.ProviderAccount) (*billing.ProviderAccount, error) {
		calls++
		require.NotNil(t, cur)
		cur.HasPaymentMethod = true
		return cur, nil
	}
	applied, err = s.ApplyEvent(ctx, "stripe", "evt_1", "invoice.paid", a, update)
	require.NoError(t, err)
	assert.True(t, applied, "a failed apply left the event unclaimed")
	applied, err = s.ApplyEvent(ctx, "stripe", "evt_1", "invoice.paid", a, update)
	require.NoError(t, err)
	assert.False(t, applied, "a repeated event is ignored")
	assert.Equal(t, 1, calls)
	got, err := s.Account(ctx, a)
	require.NoError(t, err)
	assert.True(t, got.HasPaymentMethod)

	// an event of no organization is still claimed once
	applied, err = s.ApplyEvent(ctx, "stripe", "evt_2", "customer.created", "", func(cur *billing.ProviderAccount) (*billing.ProviderAccount, error) {
		assert.Nil(t, cur)
		return nil, nil
	})
	require.NoError(t, err)
	assert.True(t, applied)
}

func TestReportsKeepTheirQuantity(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	s := Store{DB: d.App}
	a := string(repotest.TenantA)
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	now := time.Now().UTC().Truncate(time.Second)
	r := &billing.UsageReport{Organization: a, PeriodStart: start, PeriodEnd: start.AddDate(0, 1, 0), Metric: billing.MetricExtraDevices,
		Identifier: billing.UsageIdentifier(a, billing.MetricExtraDevices, start), CustomerID: "cus_a", PriceID: "price_d",
		Quantity: 12, State: billing.ReportSent, Attempts: 1, LastAttemptAt: &now, SentAt: &now}
	require.NoError(t, s.PutReport(ctx, r))
	r.State = billing.ReportConfirmed
	v := 12.0
	r.ProviderValue = &v
	require.NoError(t, s.PutReport(ctx, r))

	got, err := s.Reports(ctx, start)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, billing.ReportConfirmed, got[0].State)
	assert.Equal(t, int64(12), got[0].Quantity)
	assert.Equal(t, 12.0, *got[0].ProviderValue)
	assert.True(t, start.Equal(got[0].PeriodStart))

	changed := *r
	changed.Quantity = 13
	assert.Error(t, s.PutReport(ctx, &changed), "a report's quantity never changes")

	all, err := s.Reports(ctx, time.Time{})
	require.NoError(t, err)
	assert.Len(t, all, 1)
	other, err := s.Reports(ctx, start.AddDate(0, 1, 0))
	require.NoError(t, err)
	assert.Empty(t, other)
}

func TestNoticesAndRecipients(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	s := Store{DB: d.App}
	a := string(repotest.TenantA)
	ok, err := s.ClaimNotice(ctx, a, "payment_failed", "2026-11-03")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = s.ClaimNotice(ctx, a, "payment_failed", "2026-11-03")
	require.NoError(t, err)
	assert.False(t, ok)

	for _, u := range []struct{ name, sendTo, group string }{
		{"admin@a.test", "", "Administrators"},
		{"bob", "bob@a.test", "Administrators"},
		{"carol", "", "Administrators"},
		{"dave@a.test", "", "Users"},
	} {
		_, err := d.Owner.Exec("INSERT INTO users (tenant_id, username, two_fa_send_to) VALUES ($1, $2, $3)", a, u.name, u.sendTo)
		require.NoError(t, err)
		_, err = d.Owner.Exec("INSERT INTO user_groups (tenant_id, username, group_name) VALUES ($1, $2, $3)", a, u.name, u.group)
		require.NoError(t, err)
	}
	got, err := s.AdminEmails(ctx, a)
	require.NoError(t, err)
	assert.Equal(t, []string{"admin@a.test", "bob@a.test"}, got)
}
