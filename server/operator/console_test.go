package operator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/billing"
	"github.com/openrport/openrport/server/metering"
	"github.com/openrport/openrport/server/routes"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

func (f *fakeService) Organizations(context.Context) ([]OrganizationDetail, error) {
	f.calls = append(f.calls, "organizations")
	return f.orgs, f.err
}

func (f *fakeService) UsageDays(_ context.Context, from, to time.Time) ([]DayUsage, error) {
	f.calls = append(f.calls, "days "+from.Format(time.DateOnly)+" "+to.Format(time.DateOnly))
	return f.days, f.err
}

func (f *fakeService) AwaitingSignups(context.Context) ([]time.Time, error) {
	f.calls = append(f.calls, "signups")
	return f.waiting, f.err
}

func (f *fakeService) Devices(_ context.Context, id string) ([]Device, error) {
	f.calls = append(f.calls, "devices "+id)
	return f.devs, f.err
}

func (f *fakeService) Invitations(_ context.Context, id string) ([]Invitation, error) {
	f.calls = append(f.calls, "invitations "+id)
	return f.invs, f.err
}

func (f *fakeService) ResendInvitation(_ context.Context, by Actor, id, username string) (*Invitation, error) {
	f.calls = append(f.calls, "resend "+id+" "+username)
	f.by = by
	return &Invitation{Organization: id, Username: username, Groups: []string{}, InvitedBy: "operator:" + by.Username}, f.err
}

func (f *fakeService) RevokeInvitation(_ context.Context, by Actor, id, username string) error {
	f.calls = append(f.calls, "revoke "+id+" "+username)
	f.by = by
	return f.err
}

func (f *fakeService) Activity(_ context.Context, id string, q ActivityQuery) ([]ActivityEntry, error) {
	f.calls = append(f.calls, "activity "+id)
	f.activityQ = q
	n := min(q.Limit, len(f.entries))
	return f.entries[:n], f.err
}

// fakeUsage is a metering.Reader with fixed data.
type fakeUsage struct {
	rows  []metering.TenantRow
	days  []metering.TenantDays
	data  metering.Data
	calls []string
}

func (u *fakeUsage) Usage(_ context.Context, tid tenant.ID, q metering.Query) (metering.Data, error) {
	u.calls = append(u.calls, "usage "+string(tid)+" "+string(q.Granularity))
	return u.data, nil
}

func (u *fakeUsage) Summary(_ context.Context, from, to time.Time) ([]metering.TenantRow, []metering.TenantDays, error) {
	u.calls = append(u.calls, "summary "+from.Format(time.RFC3339)+" "+to.Format(time.RFC3339))
	return u.rows, u.days, nil
}

// fakeBilling is a billing.OperatorService that lists accounts; its other methods are not called.
type fakeBilling struct {
	billing.OperatorService
	accounts []billing.Account
}

func (b *fakeBilling) Accounts(context.Context) ([]billing.Account, error) { return b.accounts, nil }

var t0 = time.Date(2026, 11, 25, 15, 30, 0, 0, time.UTC)

func withNow(t *testing.T, at time.Time) {
	t.Helper()
	old := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = old })
}

func newConsoleRouter(o Options) *mux.Router {
	log := logger.NewLogger("test", logger.LogOutput{File: nil}, logger.LogLevelError)
	o.Respond = &api.Responder{Log: log}
	o.CurrentUser = func(context.Context) (*users.User, error) { return operatorUser.user, nil }
	h := NewHandler(o)
	m := mux.NewRouter()
	m.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(tenant.WithID(req.Context(), SystemTenant)))
		})
	})
	h.Mount(m.PathPrefix(routes.AllRoutesPrefix + RoutePrefix).Subrouter())
	return m
}

func getJSON(t *testing.T, r http.Handler, path string, v any) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, routes.AllRoutesPrefix+path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if v != nil && w.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), v), w.Body.String())
	}
	return w.Code
}

func ip(n int) *int             { return &n }
func tp(t time.Time) *time.Time { return &t }

func TestConsoleRoutesOnlyForOperators(t *testing.T) {
	paths := []struct{ method, path string }{
		{http.MethodGet, "/operator/overview"},
		{http.MethodGet, "/operator/organizations?stats=30d"},
		{http.MethodGet, "/operator/organizations/x/stats"},
		{http.MethodGet, "/operator/organizations/x/devices"},
		{http.MethodGet, "/operator/organizations/x/activity"},
		{http.MethodGet, "/operator/invitations"},
		{http.MethodPost, "/operator/organizations/x/invitations/u/resend"},
		{http.MethodDelete, "/operator/organizations/x/invitations/u"},
	}
	for _, who := range []testUser{orgAdmin, systemViewer} {
		svc := &fakeService{}
		r := newRouter(svc, who)
		for _, p := range paths {
			code, out := do(t, r, p.method, p.path, "")
			assert.Contains(t, []int{http.StatusNotFound, http.StatusForbidden}, code, "%s %s: %s", p.method, p.path, out["_raw"])
		}
		assert.Empty(t, svc.calls)
	}
}

func TestOverviewWithoutMeteringBillingSignup(t *testing.T) {
	withNow(t, t0)
	svc := &fakeService{orgs: []OrganizationDetail{
		{Organization: Organization{ID: "a", Name: "A", Status: StatusActive, DeviceCount: 9, DeviceLimit: ip(10)}, Activity: Activity{DevicesOnline: 4, InvitationsPending: 2}},
		{Organization: Organization{ID: "b", Name: "B", Status: StatusSuspended, DeviceCount: 1}, Activity: Activity{DevicesOnline: 1, InvitationsPending: 1}},
	}}
	r := newConsoleRouter(Options{Service: svc})
	var out map[string]map[string]any
	require.Equal(t, http.StatusOK, getJSON(t, r, "/operator/overview", &out))
	d := out["data"]
	assert.Equal(t, map[string]any{"total": 2.0, "active": 1.0, "suspended": 1.0, "trial": 0.0}, d["organizations"])
	assert.Equal(t, map[string]any{"online": 5.0, "total": 10.0}, d["devices"])
	assert.Equal(t, false, d["metering"])
	assert.Nil(t, d["transfer"])
	assert.Nil(t, d["series"])
	assert.Nil(t, d["signups_awaiting"])
	assert.Nil(t, d["oldest_signup_waiting_since"])
	assert.Equal(t, 3.0, d["invitations_pending"])
	assert.Equal(t, []any{map[string]any{"kind": "device_limit", "severity": "warning", "organization": map[string]any{"id": "a", "name": "A"},
		"message": "9 of 10 devices", "value": 9.0, "limit": 10.0}}, d["attention"])
	assert.Equal(t, "2026-11-25T15:30:00Z", d["generated_at"])
	assert.Equal(t, []string{"organizations"}, svc.calls, "no usage, no signups")
}

func TestOverviewWithEverything(t *testing.T) {
	withNow(t, t0)
	today := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	svc := &fakeService{
		orgs: []OrganizationDetail{
			{Organization: Organization{ID: "a", Name: "A", Status: StatusActive, DeviceCount: 10, DeviceLimit: ip(10)}, Activity: Activity{DevicesOnline: 2}},
			{Organization: Organization{ID: "b", Name: "B", Status: StatusActive, DeviceCount: 3}, Activity: Activity{DevicesOnline: 3}},
			{Organization: Organization{ID: "c", Name: "C", Status: StatusActive, DeviceCount: 0, DeviceLimit: ip(0)}},
		},
		days:    []DayUsage{{Day: today.AddDate(0, 0, -1), In: 5, Out: 7, Devices: 4}},
		waiting: []time.Time{t0.Add(-30 * time.Hour), t0.Add(-time.Hour)},
	}
	usage := &fakeUsage{
		rows: []metering.TenantRow{
			{Tenant: "a", Metric: "tunnel_bytes_to_device", Value: 100}, {Tenant: "a", Metric: "upload_bytes_to_devices", Value: 10},
			{Tenant: "a", Metric: "tunnel_bytes_to_user", Value: 200}, {Tenant: "a", Metric: "api_bytes_out", Value: 20},
			{Tenant: "_system", Metric: "api_bytes_out", Value: 1000},
		},
		days: []metering.TenantDays{{Tenant: "a", DeviceDays: 5, PeakDay: 5}, {Tenant: "b", DeviceDays: 3, PeakDay: 3}},
	}
	bill := &fakeBilling{accounts: []billing.Account{
		{Billing: billing.Billing{Organization: "a", Status: billing.StatusTrial, TrialEndsAt: tp(t0.Add(48 * time.Hour)), TransferState: billing.TransferSoft}},
		{Billing: billing.Billing{Organization: "b", Status: billing.StatusPaying, TransferState: billing.TransferCapped}},
	}}
	r := newConsoleRouter(Options{Service: svc, Usage: usage, Billing: bill, Signup: true})
	var out struct{ Data Overview }
	require.Equal(t, http.StatusOK, getJSON(t, r, "/operator/overview", &out))
	o := out.Data
	assert.Equal(t, 1, o.Organizations.Trial)
	assert.True(t, o.Metering)
	require.NotNil(t, o.Transfer)
	assert.Equal(t, Transfer{In: 110, Out: 220}, o.Transfer.Last24h, "reserved tenants are left out")
	assert.Equal(t, Transfer{In: 110, Out: 220}, o.Transfer.Last30d)
	require.Len(t, o.Series, 30)
	assert.Equal(t, "2026-10-27", o.Series[0].Day)
	assert.Equal(t, "2026-11-25", o.Series[29].Day)
	assert.Equal(t, SeriesDay{Day: "2026-11-24", In: 5, Out: 7, Devices: 4}, o.Series[28])
	assert.Equal(t, SeriesDay{Day: "2026-11-25"}, o.Series[29], "zero-filled")
	require.NotNil(t, o.SignupsAwaiting)
	assert.Equal(t, 2, *o.SignupsAwaiting)
	assert.Equal(t, t0.Add(-30*time.Hour), *o.OldestSignupWaitingSince)

	var kinds []string
	for _, a := range o.Attention {
		org := ""
		if a.Organization != nil {
			org = a.Organization.ID
		}
		kinds = append(kinds, a.Severity+" "+a.Kind+" "+org)
	}
	assert.Equal(t, []string{
		"critical device_limit a",
		"critical transfer_cap b",
		"warning transfer_cap a",
		"warning trial_ending a",
		"warning devices_drop a",
		"warning signup_waiting ",
	}, kinds, "c has a limit of 0 and no devices: nothing to see; b has 3 online of 3 yesterday")
	assert.Equal(t, []string{
		"summary 2026-11-24T16:00:00Z 2026-11-25T16:00:00Z",
		"summary 2026-10-27T00:00:00Z 2026-11-26T00:00:00Z",
		"summary 2026-11-24T00:00:00Z 2026-11-25T00:00:00Z",
	}, usage.calls)
	assert.Contains(t, svc.calls, "days 2026-10-27 2026-11-26")
}

func TestAttentionTrialEnded(t *testing.T) {
	orgs := []OrganizationDetail{{Organization: Organization{ID: "a", Name: "A"}}, {Organization: Organization{ID: "b", Name: "B"}}}
	accounts := map[string]billing.Account{
		"a": {Billing: billing.Billing{Organization: "a", Status: billing.StatusTrial, TrialEndsAt: tp(t0.Add(-time.Hour))}},
		"b": {Billing: billing.Billing{Organization: "b", Status: billing.StatusTrial, TrialEndsAt: tp(t0.Add(-time.Hour)), HasPaymentMethod: true}},
	}
	items := attention(orgs, accounts, nil, nil, true, false, false, t0)
	require.Len(t, items, 1)
	assert.Equal(t, AttentionItem{Kind: AttentionTrialEnding, Severity: SeverityCritical, Organization: &OrganizationRef{ID: "a", Name: "A"},
		Message: "Trial ended 2026-11-25 without a payment method"}, items[0])
	assert.Empty(t, attention(orgs, accounts, nil, nil, false, false, false, t0), "billing items only with billing")
}

func TestOrganizationsWithStats(t *testing.T) {
	withNow(t, t0)
	svc := &fakeService{orgs: []OrganizationDetail{
		{Organization: Organization{ID: "a", Name: "A", Status: StatusActive, DeviceCount: 2}, Activity: Activity{DevicesOnline: 1,
			LastDeviceAt: tp(t0.Add(-time.Hour)), LastLoginAt: tp(t0.Add(-2 * time.Hour)), FromSignup: true}},
		{Organization: Organization{ID: "b", Name: "B", Status: StatusActive}},
	}}
	r := newConsoleRouter(Options{Service: svc})
	var plain map[string][]map[string]any
	require.Equal(t, http.StatusOK, getJSON(t, r, "/operator/organizations", &plain))
	assert.NotContains(t, plain["data"][0], "stats")

	var out map[string][]map[string]any
	require.Equal(t, http.StatusOK, getJSON(t, r, "/operator/organizations?stats=30d", &out))
	require.Len(t, out["data"], 2)
	assert.Equal(t, map[string]any{"devices_online": 1.0, "transfer_in": nil, "transfer_out": nil, "jobs": nil,
		"last_device_at": "2026-11-25T14:30:00Z", "last_login_at": "2026-11-25T13:30:00Z", "last_activity_at": "2026-11-25T14:30:00Z",
		"source": "signup"}, out["data"][0]["stats"])
	assert.Equal(t, "operator", out["data"][1]["stats"].(map[string]any)["source"])
	assert.Nil(t, out["data"][1]["stats"].(map[string]any)["last_activity_at"])
	assert.NotContains(t, out["data"][0], "billing", "without [billing]")
	assert.Equal(t, 2.0, out["data"][0]["device_count"])

	assert.Equal(t, http.StatusBadRequest, getJSON(t, r, "/operator/organizations?stats=1y", nil))

	usage := &fakeUsage{rows: []metering.TenantRow{{Tenant: "a", Metric: "tunnel_bytes_to_device", Value: 3},
		{Tenant: "a", Metric: "api_bytes_out", Value: 4}, {Tenant: "a", Metric: "jobs_command", Value: 2}, {Tenant: "a", Metric: "jobs_script", Value: 1}}}
	bill := &fakeBilling{accounts: []billing.Account{{Billing: billing.Billing{Organization: "a", Plan: "starter", Status: billing.StatusTrial,
		TransferState: billing.TransferNormal, TrialEndsAt: tp(t0.Add(day))}}}}
	r = newConsoleRouter(Options{Service: svc, Usage: usage, Billing: bill})
	require.Equal(t, http.StatusOK, getJSON(t, r, "/operator/organizations?stats=7d", &out))
	s := out["data"][0]["stats"].(map[string]any)
	assert.Equal(t, 3.0, s["transfer_in"])
	assert.Equal(t, 4.0, s["transfer_out"])
	assert.Equal(t, 3.0, s["jobs"])
	assert.Equal(t, 0.0, out["data"][1]["stats"].(map[string]any)["transfer_in"], "metering on: zero, not null")
	assert.Equal(t, map[string]any{"plan": "starter", "status": "trial", "transfer_state": "normal", "trial_ends_at": "2026-11-26T15:30:00Z"}, out["data"][0]["billing"])
	assert.Equal(t, map[string]any{"plan": "free", "status": "paying", "transfer_state": "normal", "trial_ends_at": nil}, out["data"][1]["billing"])
	assert.Equal(t, []string{"summary 2026-11-18T15:00:00Z 2026-11-25T16:00:00Z"}, usage.calls)
}

func TestOrganizationStats(t *testing.T) {
	withNow(t, t0)
	svc := &fakeService{}
	r := newConsoleRouter(Options{Service: svc})
	var off map[string]map[string]any
	require.Equal(t, http.StatusOK, getJSON(t, r, "/operator/organizations/a/stats", &off))
	assert.Equal(t, map[string]any{"metering": false, "periods": []any{}}, off["data"])

	d1 := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	usage := &fakeUsage{data: metering.Data{
		Periods: []metering.PeriodRow{{Period: d1, Metric: "tunnel_bytes_to_device", Value: 10}, {Period: d1, Metric: "tunnel_bytes_to_user", Value: 20}},
		Days:    []metering.DayCount{{Day: d1, Devices: 3}, {Day: d1.AddDate(0, 0, 2), Devices: 5}},
	}}
	r = newConsoleRouter(Options{Service: svc, Usage: usage})
	var out struct{ Data StatsReport }
	require.Equal(t, http.StatusOK, getJSON(t, r, "/operator/organizations/a/stats?from=2026-11-01&to=2026-11-08&granularity=day", &out))
	rep := out.Data
	assert.True(t, rep.Metering)
	require.Len(t, rep.Periods, 7, "every day of the range")
	assert.Equal(t, d1.AddDate(0, 0, -1), rep.Periods[0].Start)
	assert.Equal(t, int64(10), rep.Periods[1].In)
	assert.Equal(t, int64(20), rep.Periods[1].Out)
	assert.Equal(t, int64(10), rep.Periods[1].TunnelBytesToDevice, "the raw metrics stay")
	assert.Equal(t, int64(3), rep.Periods[1].Devices)
	assert.Equal(t, int64(0), rep.Periods[2].Devices)
	assert.Equal(t, int64(5), rep.Periods[3].Devices)
	require.NotNil(t, rep.Totals)
	assert.Equal(t, int64(5), rep.Totals.Devices, "the highest")
	assert.Equal(t, int64(10), rep.Totals.In)

	require.Equal(t, http.StatusOK, getJSON(t, r, "/operator/organizations/a/stats?from=2026-11-25T10:00:00Z&to=2026-11-25T14:00:00Z&granularity=hour", &out))
	assert.Len(t, out.Data.Periods, 4)
	assert.Equal(t, http.StatusBadRequest, getJSON(t, r, "/operator/organizations/a/stats?granularity=month", nil))
	assert.Equal(t, http.StatusBadRequest, getJSON(t, r, "/operator/organizations/a/stats?from=2026-01-01&to=2026-03-01&granularity=hour", nil))
}

func TestDevicesSortedAndCounted(t *testing.T) {
	svc := &fakeService{devs: []Device{
		{ID: "3", Name: "zeta", Online: true, Version: "0.15.0", OS: "Ubuntu 24.04"},
		{ID: "1", Name: "Alpha", Version: "0.14.0", OS: "Windows"},
		{ID: "2", Name: "beta", Online: true, Version: "0.15.0", OS: "Ubuntu 24.04"},
	}}
	r := newConsoleRouter(Options{Service: svc})
	var out struct{ Data Devices }
	require.Equal(t, http.StatusOK, getJSON(t, r, "/operator/organizations/a/devices", &out))
	assert.Equal(t, 3, out.Data.Total)
	assert.Equal(t, 2, out.Data.Online)
	var ids []string
	for _, d := range out.Data.Devices {
		ids = append(ids, d.ID)
	}
	assert.Equal(t, []string{"2", "3", "1"}, ids, "online first, then by name")
	assert.Equal(t, []DeviceCount{{"0.15.0", 2}, {"0.14.0", 1}}, out.Data.ByVersion)
	assert.Equal(t, []DeviceCount{{"Ubuntu 24.04", 2}, {"Windows", 1}}, out.Data.ByOS)

	svc.devs = nil
	var raw map[string]map[string]any
	require.Equal(t, http.StatusOK, getJSON(t, r, "/operator/organizations/a/devices", &raw))
	assert.Equal(t, map[string]any{"total": 0.0, "online": 0.0, "devices": []any{}, "by_version": []any{}, "by_os": []any{}}, raw["data"])
}

func TestInvitationRoutes(t *testing.T) {
	svc := &fakeService{}
	r := newConsoleRouter(Options{Service: svc})
	var out map[string]any
	require.Equal(t, http.StatusOK, getJSON(t, r, "/operator/invitations?organization=a", &out))
	assert.Equal(t, []any{}, out["data"])
	code, body := do(t, r, http.MethodPost, "/operator/organizations/a/invitations/bob@example.com/resend", "")
	require.Equal(t, http.StatusOK, code, body["_raw"])
	assert.Equal(t, "operator:op", body["data"].(map[string]any)["invited_by"])
	code, _ = do(t, r, http.MethodDelete, "/operator/organizations/a/invitations/bob@example.com", "")
	assert.Equal(t, http.StatusNoContent, code)
	assert.Equal(t, []string{"invitations a", "resend a bob@example.com", "revoke a bob@example.com"}, svc.calls)

	svc = &fakeService{err: ErrNoInvitation}
	r = newConsoleRouter(Options{Service: svc})
	code, _ = do(t, r, http.MethodDelete, "/operator/organizations/a/invitations/bob", "")
	assert.Equal(t, http.StatusNotFound, code)
}

func TestActivityPaging(t *testing.T) {
	var entries []ActivityEntry
	for i := 5; i > 0; i-- {
		entries = append(entries, ActivityEntry{ID: int64(i), Timestamp: t0.Add(time.Duration(i) * time.Second), Source: SourceOrganization})
	}
	svc := &fakeService{entries: entries}
	r := newConsoleRouter(Options{Service: svc})
	var out struct {
		Data []ActivityEntry
		Next *string
	}
	require.Equal(t, http.StatusOK, getJSON(t, r, "/operator/organizations/a/activity?kind=sign_in&limit=2", &out))
	require.Len(t, out.Data, 2)
	require.NotNil(t, out.Next)
	assert.Equal(t, ActivitySignIn, svc.activityQ.Kind)
	assert.Equal(t, 3, svc.activityQ.Limit, "one more than the page")

	require.Equal(t, http.StatusOK, getJSON(t, r, "/operator/organizations/a/activity?before="+*out.Next, &out))
	require.NotNil(t, svc.activityQ.Before)
	assert.Equal(t, int64(4), svc.activityQ.Before.ID)
	assert.True(t, svc.activityQ.Before.Timestamp.Equal(t0.Add(4*time.Second)))
	assert.Nil(t, out.Next, "all of them fit")
	assert.Len(t, out.Data, 5)

	for _, bad := range []string{"kind=x", "limit=0", "limit=501", "before=nope", "before=1_x"} {
		assert.Equal(t, http.StatusBadRequest, getJSON(t, r, "/operator/organizations/a/activity?"+bad, nil), bad)
	}
	var raw map[string]any
	svc.entries = nil
	require.Equal(t, http.StatusOK, getJSON(t, r, "/operator/organizations/a/activity", &raw))
	assert.Equal(t, []any{}, raw["data"])
	assert.Nil(t, raw["next"])
	assert.True(t, strings.HasPrefix(encodeCursor(entries[0]), "1"))
}
