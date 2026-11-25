package operator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/billing"
	"github.com/openrport/openrport/server/metering"
	"github.com/openrport/openrport/share/tenant"
)

// The operator console's routes: the platform overview, organizations with their stats, and per organization its
// usage series, devices, invitations and activity. Metadata only: never device contents, command output, files,
// vault values or audit log request/response bodies.

const (
	day = 24 * time.Hour
	// seriesDays is how many UTC days the overview's series covers (today included).
	seriesDays = 30
	// signupWaitAlert is how long a signup may wait for approval before the overview lists it.
	signupWaitAlert = 24 * time.Hour
	// trialEndingSoon is how close to its end a trial is listed.
	trialEndingSoon = 3 * 24 * time.Hour
	// activityLimit and maxActivityLimit are the default and the largest page of activity.
	activityLimit    = 100
	maxActivityLimit = 500
)

// mountConsole registers the console's routes.
func (h *Handler) mountConsole(r *mux.Router) {
	r.HandleFunc("/overview", h.overview).Methods(http.MethodGet)
	r.HandleFunc("/organizations/{id}/stats", h.organizationStats).Methods(http.MethodGet)
	r.HandleFunc("/organizations/{id}/devices", h.devices).Methods(http.MethodGet)
	r.HandleFunc("/organizations/{id}/activity", h.activity).Methods(http.MethodGet)
	r.HandleFunc("/invitations", h.invitations).Methods(http.MethodGet)
	r.HandleFunc("/organizations/{id}/invitations/{username}/resend", h.resendInvitation).Methods(http.MethodPost)
	r.HandleFunc("/organizations/{id}/invitations/{username}", h.revokeInvitation).Methods(http.MethodDelete)
}

// Transfer is bytes to the devices (In) and from them (Out).
type Transfer struct {
	In  int64 `json:"in"`
	Out int64 `json:"out"`
}

func transferOf(t metering.Totals) Transfer {
	return Transfer{In: t.TunnelBytesToDevice + t.UploadBytesToDevices, Out: t.TunnelBytesToUser + t.APIBytesOut}
}

// OrganizationRef names an organization in the overview.
type OrganizationRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Attention kinds and severities.
const (
	AttentionDeviceLimit   = "device_limit"
	AttentionTransferCap   = "transfer_cap"
	AttentionTrialEnding   = "trial_ending"
	AttentionDevicesDrop   = "devices_drop"
	AttentionSignupWaiting = "signup_waiting"

	SeverityCritical = "critical"
	SeverityWarning  = "warning"
)

var attentionOrder = map[string]int{AttentionDeviceLimit: 0, AttentionTransferCap: 1, AttentionTrialEnding: 2, AttentionDevicesDrop: 3, AttentionSignupWaiting: 4}

// AttentionItem is something an operator should look at. Organization is nil for platform-wide items.
type AttentionItem struct {
	Kind         string           `json:"kind"`
	Severity     string           `json:"severity"`
	Organization *OrganizationRef `json:"organization,omitempty"`
	Message      string           `json:"message"`
	Value        *int64           `json:"value,omitempty"`
	Limit        *int64           `json:"limit,omitempty"`
}

// SeriesDay is one UTC day of the overview's series.
type SeriesDay struct {
	Day     string `json:"day"`
	In      int64  `json:"in"`
	Out     int64  `json:"out"`
	Devices int64  `json:"devices"`
}

// OrganizationTotals counts the organizations by status.
type OrganizationTotals struct {
	Total     int `json:"total"`
	Active    int `json:"active"`
	Suspended int `json:"suspended"`
	// Trial is the organizations on billing status trial (0 without [billing]).
	Trial int `json:"trial"`
}

// DeviceTotals counts the devices of every organization.
type DeviceTotals struct {
	Online int `json:"online"`
	Total  int `json:"total"`
}

// TransferTotals is the platform's transfer over the last day and the last 30 days.
type TransferTotals struct {
	Last24h Transfer `json:"last_24h"`
	Last30d Transfer `json:"last_30d"`
}

// Overview is the platform overview (GET /operator/overview).
type Overview struct {
	GeneratedAt   time.Time          `json:"generated_at"`
	Organizations OrganizationTotals `json:"organizations"`
	Devices       DeviceTotals       `json:"devices"`
	Metering      bool               `json:"metering"`
	// Transfer and Series are nil without usage metering.
	Transfer *TransferTotals `json:"transfer"`
	// SignupsAwaiting and OldestSignupWaitingSince are nil without [signup].
	SignupsAwaiting          *int            `json:"signups_awaiting"`
	OldestSignupWaitingSince *time.Time      `json:"oldest_signup_waiting_since"`
	InvitationsPending       int             `json:"invitations_pending"`
	Attention                []AttentionItem `json:"attention"`
	Series                   []SeriesDay     `json:"series"`
}

func startOfDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// sumTransfer adds up the transfer of the organizations (not the reserved tenants) in a usage summary.
func sumTransfer(rows []metering.TenantRow) Transfer {
	var out Transfer
	for id, t := range metering.TenantTotals(rows, nil, time.Time{}, time.Time{}, time.Time{}) {
		if !strings.HasPrefix(id, "_") {
			tr := transferOf(t)
			out.In += tr.In
			out.Out += tr.Out
		}
	}
	return out
}

func (h *Handler) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t := now().UTC()
	orgs, err := h.svc.Organizations(ctx)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := Overview{GeneratedAt: t, Metering: h.usage != nil, Attention: []AttentionItem{}}
	var accounts map[string]billing.Account
	if h.billing != nil {
		if accounts, err = h.billingByOrganization(ctx); err != nil {
			h.fail(w, err)
			return
		}
	}
	for _, o := range orgs {
		out.Organizations.Total++
		switch o.Status {
		case StatusActive:
			out.Organizations.Active++
		case StatusSuspended:
			out.Organizations.Suspended++
		}
		if a, ok := accounts[o.ID]; ok && a.Status == billing.StatusTrial {
			out.Organizations.Trial++
		}
		out.Devices.Total += o.DeviceCount
		out.Devices.Online += o.Activity.DevicesOnline
		out.InvitationsPending += o.Activity.InvitationsPending
	}
	var yesterday map[string]int64
	if h.usage != nil {
		if yesterday, err = h.fillUsage(ctx, &out, t); err != nil {
			h.fail(w, err)
			return
		}
	}
	var waiting []time.Time
	if h.signup {
		if waiting, err = h.svc.AwaitingSignups(ctx); err != nil {
			h.fail(w, err)
			return
		}
		n := len(waiting)
		out.SignupsAwaiting = &n
		if n > 0 {
			oldest := waiting[0]
			for _, w := range waiting {
				if w.Before(oldest) {
					oldest = w
				}
			}
			out.OldestSignupWaitingSince = &oldest
		}
	}
	out.Attention = attention(orgs, accounts, yesterday, waiting, h.billing != nil, h.usage != nil, h.signup, t)
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(out))
}

// fillUsage sets the overview's transfer and series, and returns each organization's daily device count of
// yesterday.
func (h *Handler) fillUsage(ctx context.Context, out *Overview, t time.Time) (map[string]int64, error) {
	hour := t.Truncate(time.Hour)
	rows, _, err := h.usage.Summary(ctx, hour.Add(-23*time.Hour), hour.Add(time.Hour))
	if err != nil {
		return nil, err
	}
	out.Transfer = &TransferTotals{Last24h: sumTransfer(rows)}
	today := startOfDay(t)
	first := today.AddDate(0, 0, -(seriesDays - 1))
	if rows, _, err = h.usage.Summary(ctx, first, today.AddDate(0, 0, 1)); err != nil {
		return nil, err
	}
	out.Transfer.Last30d = sumTransfer(rows)
	days, err := h.svc.UsageDays(ctx, first, today.AddDate(0, 0, 1))
	if err != nil {
		return nil, err
	}
	byDay := map[time.Time]DayUsage{}
	for _, d := range days {
		byDay[startOfDay(d.Day)] = d
	}
	out.Series = make([]SeriesDay, 0, seriesDays)
	for d := first; !d.After(today); d = d.AddDate(0, 0, 1) {
		u := byDay[d]
		out.Series = append(out.Series, SeriesDay{Day: d.Format(time.DateOnly), In: u.In, Out: u.Out, Devices: u.Devices})
	}
	_, ydays, err := h.usage.Summary(ctx, today.AddDate(0, 0, -1), today)
	if err != nil {
		return nil, err
	}
	yesterday := map[string]int64{}
	for _, d := range ydays {
		yesterday[d.Tenant] = d.PeakDay
	}
	return yesterday, nil
}

func (h *Handler) billingByOrganization(ctx context.Context) (map[string]billing.Account, error) {
	as, err := h.billing.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]billing.Account, len(as))
	for _, a := range as {
		out[a.Organization] = a
	}
	return out, nil
}

func i64(n int64) *int64 { return &n }

// attention lists what needs an operator's attention, most severe first.
func attention(orgs []OrganizationDetail, accounts map[string]billing.Account, yesterday map[string]int64, waiting []time.Time,
	withBilling, withMetering, withSignup bool, t time.Time) []AttentionItem {
	out := []AttentionItem{}
	for _, o := range orgs {
		ref := &OrganizationRef{ID: o.ID, Name: o.Name}
		if l := o.DeviceLimit; l != nil && (*l > 0 || o.DeviceCount > *l) && o.DeviceCount*10 >= *l*9 {
			sev := SeverityWarning
			if o.DeviceCount >= *l {
				sev = SeverityCritical
			}
			out = append(out, AttentionItem{Kind: AttentionDeviceLimit, Severity: sev, Organization: ref,
				Message: fmt.Sprintf("%d of %d devices", o.DeviceCount, *l), Value: i64(int64(o.DeviceCount)), Limit: i64(int64(*l))})
		}
		if withBilling {
			if a, ok := accounts[o.ID]; ok {
				switch a.TransferState {
				case billing.TransferSoft:
					out = append(out, AttentionItem{Kind: AttentionTransferCap, Severity: SeverityWarning, Organization: ref,
						Message: "Transfer reached the soft cap"})
				case billing.TransferOver:
					out = append(out, AttentionItem{Kind: AttentionTransferCap, Severity: SeverityCritical, Organization: ref,
						Message: "Transfer is over the plan's allowance"})
				case billing.TransferCapped:
					out = append(out, AttentionItem{Kind: AttentionTransferCap, Severity: SeverityCritical, Organization: ref,
						Message: "Transfer is at the hard cap: tunnels are throttled"})
				}
				if a.Status == billing.StatusTrial && a.TrialEndsAt != nil {
					switch ends := a.TrialEndsAt.UTC(); {
					case !ends.After(t) && !a.HasPaymentMethod:
						out = append(out, AttentionItem{Kind: AttentionTrialEnding, Severity: SeverityCritical, Organization: ref,
							Message: "Trial ended " + ends.Format(time.DateOnly) + " without a payment method"})
					case ends.After(t) && ends.Sub(t) <= trialEndingSoon:
						out = append(out, AttentionItem{Kind: AttentionTrialEnding, Severity: SeverityWarning, Organization: ref,
							Message: "Trial ends " + ends.Format(time.DateOnly)})
					}
				}
			}
		}
		if withMetering {
			if y := yesterday[o.ID]; y >= 2 && int64(o.Activity.DevicesOnline)*2 <= y {
				out = append(out, AttentionItem{Kind: AttentionDevicesDrop, Severity: SeverityWarning, Organization: ref,
					Message: fmt.Sprintf("%d devices online now, %d yesterday", o.Activity.DevicesOnline, y),
					Value:   i64(int64(o.Activity.DevicesOnline)), Limit: i64(y)})
			}
		}
	}
	if withSignup {
		var n int64
		for _, since := range waiting {
			if t.Sub(since) > signupWaitAlert {
				n++
			}
		}
		if n > 0 {
			msg := fmt.Sprintf("%d signups waiting for approval for more than 24 h", n)
			if n == 1 {
				msg = "1 signup waiting for approval for more than 24 h"
			}
			out = append(out, AttentionItem{Kind: AttentionSignupWaiting, Severity: SeverityWarning, Message: msg, Value: i64(n)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Severity != b.Severity {
			return a.Severity == SeverityCritical
		}
		if attentionOrder[a.Kind] != attentionOrder[b.Kind] {
			return attentionOrder[a.Kind] < attentionOrder[b.Kind]
		}
		return a.Organization != nil && b.Organization != nil && strings.ToLower(a.Organization.Name) < strings.ToLower(b.Organization.Name)
	})
	return out
}

// statsRanges are the ranges GET /organizations?stats= takes.
var statsRanges = map[string]int{"7d": 7, "30d": 30, "90d": 90}

// OrganizationStats are an organization's aggregates over the range of GET /organizations?stats=.
type OrganizationStats struct {
	DevicesOnline int `json:"devices_online"`
	// TransferIn, TransferOut and Jobs are nil without usage metering.
	TransferIn  *int64 `json:"transfer_in"`
	TransferOut *int64 `json:"transfer_out"`
	Jobs        *int64 `json:"jobs"`
	// LastDeviceAt is the newest device connect or disconnect, LastLoginAt the newest member sign-in,
	// LastActivityAt the later of the two.
	LastDeviceAt   *time.Time `json:"last_device_at"`
	LastLoginAt    *time.Time `json:"last_login_at"`
	LastActivityAt *time.Time `json:"last_activity_at"`
	// Source is "signup" (a completed signup created it) or "operator".
	Source string `json:"source"`
}

// OrganizationBilling is the billing summary of an organization in the list ([billing] only).
type OrganizationBilling struct {
	Plan          string     `json:"plan"`
	Status        string     `json:"status"`
	TransferState string     `json:"transfer_state"`
	TrialEndsAt   *time.Time `json:"trial_ends_at"`
}

// OrganizationWithStats is an organization of GET /organizations?stats=.
type OrganizationWithStats struct {
	Organization
	Stats   OrganizationStats    `json:"stats"`
	Billing *OrganizationBilling `json:"billing,omitempty"`
}

// listWithStats answers GET /organizations?stats=7d|30d|90d.
func (h *Handler) listWithStats(w http.ResponseWriter, r *http.Request, rng string) {
	days, ok := statsRanges[rng]
	if !ok {
		h.fail(w, fmt.Errorf("%w: stats must be 7d, 30d or 90d", ErrInvalid))
		return
	}
	ctx := r.Context()
	orgs, err := h.svc.Organizations(ctx)
	if err != nil {
		h.fail(w, err)
		return
	}
	var totals map[string]metering.Totals
	if h.usage != nil {
		t := now().UTC()
		from, to := t.Add(-time.Duration(days)*day).Truncate(time.Hour), t.Truncate(time.Hour).Add(time.Hour)
		rows, ds, err := h.usage.Summary(ctx, from, to)
		if err != nil {
			h.fail(w, err)
			return
		}
		totals = metering.TenantTotals(rows, ds, from, to, t)
	}
	var accounts map[string]billing.Account
	if h.billing != nil {
		if accounts, err = h.billingByOrganization(ctx); err != nil {
			h.fail(w, err)
			return
		}
	}
	out := make([]OrganizationWithStats, 0, len(orgs))
	for _, o := range orgs {
		s := OrganizationStats{DevicesOnline: o.Activity.DevicesOnline, LastDeviceAt: o.Activity.LastDeviceAt,
			LastLoginAt: o.Activity.LastLoginAt, LastActivityAt: later(o.Activity.LastDeviceAt, o.Activity.LastLoginAt), Source: "operator"}
		if o.Activity.FromSignup {
			s.Source = "signup"
		}
		if totals != nil {
			tot := totals[o.ID]
			tr := transferOf(tot)
			s.TransferIn, s.TransferOut, s.Jobs = i64(tr.In), i64(tr.Out), i64(tot.JobsRun)
		}
		item := OrganizationWithStats{Organization: o.Organization, Stats: s}
		if accounts != nil {
			// an organization without a billing record is on the free plan
			b := OrganizationBilling{Plan: "free", Status: string(billing.StatusPaying), TransferState: string(billing.TransferNormal)}
			if a, ok := accounts[o.ID]; ok {
				b = OrganizationBilling{Plan: a.Plan, Status: string(a.Status), TransferState: string(a.TransferState), TrialEndsAt: a.TrialEndsAt}
				if b.TransferState == "" {
					b.TransferState = string(billing.TransferNormal)
				}
			}
			item.Billing = &b
		}
		out = append(out, item)
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(out))
}

func later(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil || a.After(*b):
		return a
	}
	return b
}

// StatsTotals are a stats report's totals with the transfer directions and the highest device count.
type StatsTotals struct {
	metering.Totals
	In      int64 `json:"in"`
	Out     int64 `json:"out"`
	Devices int64 `json:"devices"`
}

// StatsPeriod is a period of a stats report.
type StatsPeriod struct {
	Start time.Time `json:"start"`
	StatsTotals
}

// StatsReport is GET /organizations/{id}/stats: the usage report with every period of the range present.
type StatsReport struct {
	Metering    bool                      `json:"metering"`
	From        *time.Time                `json:"from,omitempty"`
	To          *time.Time                `json:"to,omitempty"`
	Granularity metering.Granularity      `json:"granularity,omitempty"`
	Periods     []StatsPeriod             `json:"periods"`
	Totals      *StatsTotals              `json:"totals,omitempty"`
	TopDevices  []metering.ClientTransfer `json:"top_devices,omitempty"`
}

func statsTotals(t metering.Totals, g metering.Granularity) StatsTotals {
	tr := transferOf(t)
	devices := t.DevicesPeakDay
	if g == metering.Hour {
		devices = t.DevicesPeak
	}
	return StatsTotals{Totals: t, In: tr.In, Out: tr.Out, Devices: devices}
}

// organizationStats answers GET /organizations/{id}/stats?from&to&granularity=hour|day.
func (h *Handler) organizationStats(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if _, err := h.svc.Get(r.Context(), id); err != nil {
		h.fail(w, err)
		return
	}
	if h.usage == nil {
		h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(StatsReport{Metering: false, Periods: []StatsPeriod{}}))
		return
	}
	qv := r.URL.Query()
	g := qv.Get("granularity")
	if g != "" && g != string(metering.Hour) && g != string(metering.Day) {
		h.fail(w, fmt.Errorf("%w: granularity must be hour or day", ErrInvalid))
		return
	}
	t := now()
	q, err := metering.ParseQuery(qv.Get("from"), qv.Get("to"), g, t)
	if err != nil {
		h.fail(w, err)
		return
	}
	d, err := h.usage.Usage(r.Context(), tenant.ID(id), q)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(buildStats(q, metering.BuildReport(q, d, t))))
}

// buildStats fills the periods of the range BuildReport left out (no usage) and adds in, out and devices.
func buildStats(q metering.Query, rep metering.Report) StatsReport {
	step := func(t time.Time) time.Time { return t.Add(time.Hour) }
	if q.Granularity == metering.Day {
		step = func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }
	}
	have := make(map[time.Time]metering.Period, len(rep.Periods))
	for _, p := range rep.Periods {
		have[p.Start.UTC()] = p
	}
	from, to := rep.From.UTC(), rep.To.UTC()
	out := StatsReport{Metering: true, From: &from, To: &to, Granularity: rep.Granularity, Periods: []StatsPeriod{}, TopDevices: rep.TopDevices}
	totals := statsTotals(rep.Totals, q.Granularity)
	totals.Devices = 0
	for s := from; s.Before(to); s = step(s) {
		p := StatsPeriod{Start: s, StatsTotals: statsTotals(have[s].Totals, q.Granularity)}
		totals.Devices = max(totals.Devices, p.Devices)
		out.Periods = append(out.Periods, p)
	}
	out.Totals = &totals
	return out
}

// DeviceCount is how many devices have a value (a version, an OS).
type DeviceCount struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// Devices is GET /organizations/{id}/devices.
type Devices struct {
	Total     int           `json:"total"`
	Online    int           `json:"online"`
	Devices   []Device      `json:"devices"`
	ByVersion []DeviceCount `json:"by_version"`
	ByOS      []DeviceCount `json:"by_os"`
}

func countBy(ds []Device, value func(Device) string) []DeviceCount {
	n := map[string]int{}
	for _, d := range ds {
		n[value(d)]++
	}
	out := make([]DeviceCount, 0, len(n))
	for v, c := range n {
		out = append(out, DeviceCount{Value: v, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Value < out[j].Value
	})
	return out
}

func (h *Handler) devices(w http.ResponseWriter, r *http.Request) {
	ds, err := h.svc.Devices(r.Context(), mux.Vars(r)["id"])
	if err != nil {
		h.fail(w, err)
		return
	}
	if ds == nil {
		ds = []Device{}
	}
	sort.SliceStable(ds, func(i, j int) bool {
		if ds[i].Online != ds[j].Online {
			return ds[i].Online
		}
		a, b := strings.ToLower(ds[i].Name), strings.ToLower(ds[j].Name)
		if a != b {
			return a < b
		}
		return ds[i].ID < ds[j].ID
	})
	out := Devices{Total: len(ds), Devices: ds,
		ByVersion: countBy(ds, func(d Device) string { return d.Version }),
		ByOS:      countBy(ds, func(d Device) string { return d.OS })}
	for _, d := range ds {
		if d.Online {
			out.Online++
		}
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(out))
}

func (h *Handler) invitations(w http.ResponseWriter, r *http.Request) {
	invs, err := h.svc.Invitations(r.Context(), r.URL.Query().Get("organization"))
	if err != nil {
		h.fail(w, err)
		return
	}
	if invs == nil {
		invs = []Invitation{}
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(invs))
}

func (h *Handler) resendInvitation(w http.ResponseWriter, r *http.Request) {
	v := mux.Vars(r)
	inv, err := h.svc.ResendInvitation(r.Context(), actor(r), v["id"], v["username"])
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(inv))
}

func (h *Handler) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	v := mux.Vars(r)
	if err := h.svc.RevokeInvitation(r.Context(), actor(r), v["id"], v["username"]); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ActivityPage is GET /organizations/{id}/activity: {"data": [...], "next": cursor or null}.
type ActivityPage struct {
	Data []ActivityEntry `json:"data"`
	Next *string         `json:"next"`
}

func encodeCursor(e ActivityEntry) string {
	return strconv.FormatInt(e.Timestamp.UnixMicro(), 10) + "_" + strconv.FormatInt(e.ID, 10)
}

var errCursor = errors.New("before is not a cursor of this list")

func decodeCursor(s string) (*ActivityCursor, error) {
	ts, id, ok := strings.Cut(s, "_")
	if !ok {
		return nil, errCursor
	}
	us, err1 := strconv.ParseInt(ts, 10, 64)
	n, err2 := strconv.ParseInt(id, 10, 64)
	if err1 != nil || err2 != nil {
		return nil, errCursor
	}
	return &ActivityCursor{Timestamp: time.UnixMicro(us).UTC(), ID: n}, nil
}

func (h *Handler) activity(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	q := ActivityQuery{Kind: ActivityKind(qv.Get("kind")), Limit: activityLimit}
	switch q.Kind {
	case "", ActivityAll, ActivitySignIn, ActivityAdmin:
	default:
		h.fail(w, fmt.Errorf("%w: kind must be all, sign_in or admin", ErrInvalid))
		return
	}
	if s := qv.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxActivityLimit {
			h.fail(w, fmt.Errorf("%w: limit must be 1 to %d", ErrInvalid, maxActivityLimit))
			return
		}
		q.Limit = n
	}
	if s := qv.Get("before"); s != "" {
		c, err := decodeCursor(s)
		if err != nil {
			h.fail(w, fmt.Errorf("%w: %v", ErrInvalid, err))
			return
		}
		q.Before = c
	}
	limit := q.Limit
	q.Limit++ // one more tells whether there is a next page
	es, err := h.svc.Activity(r.Context(), mux.Vars(r)["id"], q)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := ActivityPage{Data: es}
	if len(es) > limit {
		out.Data = es[:limit]
		next := encodeCursor(es[limit-1])
		out.Next = &next
	}
	if out.Data == nil {
		out.Data = []ActivityEntry{}
	}
	h.respond.JSON(w, http.StatusOK, out)
}
