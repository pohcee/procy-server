package metering

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/openrport/openrport/share/tenant"
)

// Granularity is the length of the periods of a usage report.
type Granularity string

// The granularities.
const (
	Hour  Granularity = "hour"
	Day   Granularity = "day"
	Month Granularity = "month"
)

// maxSpan is the longest range a report of each granularity covers.
var maxSpan = map[Granularity]time.Duration{
	Hour:  31 * 24 * time.Hour,
	Day:   400 * 24 * time.Hour,
	Month: 5 * 366 * 24 * time.Hour,
}

// ErrInvalid is wrapped by the errors of invalid report parameters.
var ErrInvalid = errors.New("invalid usage query")

// PeriodRow is the value of a metric in a period (as the store aggregates usage: sums, or the highest value for
// gauges).
type PeriodRow struct {
	Period time.Time `db:"period"`
	Metric string    `db:"metric"`
	Value  int64     `db:"value"`
}

// DayCount is the number of devices connected on a UTC day (usage_device_days).
type DayCount struct {
	Day     time.Time `db:"day"`
	Devices int64     `db:"devices"`
}

// ClientRow is the value of a transfer metric of one client over a range (usage_client_daily).
type ClientRow struct {
	Client string `db:"client_id"`
	Metric string `db:"metric"`
	Value  int64  `db:"value"`
}

// TenantRow is the value of a metric in a tenant over a range.
type TenantRow struct {
	Tenant string `db:"tenant_id"`
	Metric string `db:"metric"`
	Value  int64  `db:"value"`
}

// TenantDays are a tenant's device-days over a range: the sum of the daily device counts and the highest one.
type TenantDays struct {
	Tenant     string `db:"tenant_id"`
	DeviceDays int64  `db:"device_days"`
	PeakDay    int64  `db:"peak_day"`
}

// Data is what a Reader returns for one tenant's report.
type Data struct {
	Periods []PeriodRow
	Days    []DayCount
	// Top are the transfer metrics of the clients with the most transfer in the range.
	Top []ClientRow
}

// TopClients is how many clients a report lists by transfer.
const TopClients = 10

// Reader reads the stored usage.
type Reader interface {
	// Usage returns the usage of tid in q (hours starting in [q.From, q.To)) per period of q.Granularity.
	Usage(ctx context.Context, tid tenant.ID, q Query) (Data, error)
	// Summary returns the usage of every tenant between from and to.
	Summary(ctx context.Context, from, to time.Time) ([]TenantRow, []TenantDays, error)
}

// Totals are the usage of a period (or of a whole report).
type Totals struct {
	TunnelBytesToUser    int64 `json:"tunnel_bytes_to_user"`
	TunnelBytesToDevice  int64 `json:"tunnel_bytes_to_device"`
	UploadBytesToDevices int64 `json:"upload_bytes_to_devices"`
	// TransferBytes is what plans count as transfer: tunnel bytes both ways and upload bytes delivered.
	TransferBytes int64 `json:"transfer_bytes"`
	APIBytesOut   int64 `json:"api_bytes_out"`
	JobsRun       int64 `json:"jobs_run"`
	JobsCommand   int64 `json:"jobs_command"`
	JobsScript    int64 `json:"jobs_script"`
	JobsLibrary   int64 `json:"jobs_library"`
	// DeviceHours is the connected devices integrated over the period, in hours.
	DeviceHours float64 `json:"device_hours"`
	// DevicesAvg is the average number of connected devices over the part of the period that has passed.
	DevicesAvg float64 `json:"devices_avg"`
	// DevicesPeak is the most devices connected at once (sampled per node; see the Postgres docs).
	DevicesPeak int64 `json:"devices_peak"`
	// DeviceDays is the sum over the days of the devices that had a session that day (not in hourly periods).
	DeviceDays int64 `json:"device_days"`
	// DevicesBillable is the average of the daily device counts over the days passed, rounded up (not in hourly
	// periods).
	DevicesBillable int64 `json:"devices_billable"`
	// DevicesPeakDay is the highest daily device count (not in hourly periods).
	DevicesPeakDay int64 `json:"devices_peak_day"`
	MonitoringRows int64 `json:"monitoring_rows"`

	deviceSeconds int64
}

func (t *Totals) add(metric string, v int64) {
	switch Metric(metric) {
	case TunnelBytesToUser:
		t.TunnelBytesToUser += v
	case TunnelBytesToDevice:
		t.TunnelBytesToDevice += v
	case UploadBytesToDevices:
		t.UploadBytesToDevices += v
	case APIBytesOut:
		t.APIBytesOut += v
	case JobsCommand:
		t.JobsCommand += v
	case JobsScript:
		t.JobsScript += v
	case JobsLibrary:
		t.JobsLibrary += v
	case DeviceSeconds:
		t.deviceSeconds += v
	case DevicesPeak:
		t.DevicesPeak = max(t.DevicesPeak, v)
	case MonitoringRows:
		t.MonitoringRows = max(t.MonitoringRows, v)
	}
	if IsTransfer(Metric(metric)) {
		t.TransferBytes += v
	}
	switch Metric(metric) {
	case JobsCommand, JobsScript, JobsLibrary:
		t.JobsRun += v
	}
}

func (t *Totals) addDay(devices int64) {
	t.DeviceDays += devices
	t.DevicesPeakDay = max(t.DevicesPeakDay, devices)
}

// finish fills the device figures for a span (the part of the period that has passed).
func (t *Totals) finish(span time.Duration, days bool) {
	t.DeviceHours = round3(float64(t.deviceSeconds) / 3600)
	if span > 0 {
		t.DevicesAvg = round3(float64(t.deviceSeconds) / span.Seconds())
	}
	if days {
		n := int64(math.Ceil(span.Hours() / 24))
		if n > 0 {
			t.DevicesBillable = (t.DeviceDays + n - 1) / n
		}
	}
}

func round3(f float64) float64 {
	return math.Round(f*1000) / 1000
}

// ClientTransfer is a client's transfer over a report's range.
type ClientTransfer struct {
	ClientID             string `json:"client_id"`
	TunnelBytesToUser    int64  `json:"tunnel_bytes_to_user"`
	TunnelBytesToDevice  int64  `json:"tunnel_bytes_to_device"`
	UploadBytesToDevices int64  `json:"upload_bytes_to_devices"`
	TransferBytes        int64  `json:"transfer_bytes"`
}

// Period is the usage of one period.
type Period struct {
	Start time.Time `json:"start"`
	Totals
}

// Report is a tenant's usage between From and To.
type Report struct {
	From        time.Time   `json:"from"`
	To          time.Time   `json:"to"`
	Granularity Granularity `json:"granularity"`
	// Periods are the periods with usage, oldest first.
	Periods []Period `json:"periods"`
	Totals  Totals   `json:"totals"`
	// TopDevices are the clients with the most transfer in the range (by UTC day), most first.
	TopDevices []ClientTransfer `json:"top_devices"`
}

// Query is a validated report range.
type Query struct {
	From, To    time.Time
	Granularity Granularity
}

// ParseQuery reads from, to and granularity (query parameters). from and to are RFC 3339 times or dates
// (YYYY-MM-DD, midnight UTC); to defaults to now and from to the start of to's month; granularity to "day". The
// range is widened to whole hours, and to whole UTC days for days and months.
func ParseQuery(from, to, granularity string, now time.Time) (Query, error) {
	q := Query{Granularity: Granularity(granularity)}
	if q.Granularity == "" {
		q.Granularity = Day
	}
	span, ok := maxSpan[q.Granularity]
	if !ok {
		return q, fmt.Errorf(`%w: granularity must be "hour", "day" or "month"`, ErrInvalid)
	}
	var err error
	q.To = now.UTC()
	if to != "" {
		if q.To, err = parseTime(to); err != nil {
			return q, fmt.Errorf("%w: to: %v", ErrInvalid, err)
		}
	}
	last := q.To.Add(-time.Nanosecond) // to is excluded: ?to=2026-12-01 means November
	q.From = time.Date(last.Year(), last.Month(), 1, 0, 0, 0, 0, time.UTC)
	if from != "" {
		if q.From, err = parseTime(from); err != nil {
			return q, fmt.Errorf("%w: from: %v", ErrInvalid, err)
		}
	}
	// whole hours, and whole days for days and months (the rollups are per day)
	unit := time.Hour
	if q.Granularity != Hour {
		unit = 24 * time.Hour
	}
	q.From = q.From.Truncate(unit)
	if q.To.Truncate(unit) != q.To {
		q.To = q.To.Truncate(unit).Add(unit) // include the hour (day) to falls in
	}
	if !q.From.Before(q.To) {
		return q, fmt.Errorf("%w: from must be before to", ErrInvalid)
	}
	if q.To.Sub(q.From) > span {
		return q, fmt.Errorf("%w: a report by %s covers at most %d days", ErrInvalid, q.Granularity, int(span.Hours()/24))
	}
	return q, nil
}

func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("expected an RFC 3339 time or a date (YYYY-MM-DD), got %q", s)
	}
	return t, nil
}

// ParseMonth reads a month (YYYY-MM; the current month when empty) and returns its range.
func ParseMonth(month string, now time.Time) (from, to time.Time, err error) {
	if month == "" {
		now = now.UTC()
		from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	} else {
		from, err = time.Parse("2006-01", strings.TrimSpace(month))
		if err != nil {
			return from, to, fmt.Errorf("%w: month must be YYYY-MM, got %q", ErrInvalid, month)
		}
	}
	return from, from.AddDate(0, 1, 0), nil
}

// periodStart returns the start of the period of g containing t (UTC).
func periodStart(t time.Time, g Granularity) time.Time {
	t = t.UTC()
	switch g {
	case Month:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	case Day:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	default:
		return t.Truncate(time.Hour)
	}
}

func periodEnd(start time.Time, g Granularity) time.Time {
	switch g {
	case Month:
		return start.AddDate(0, 1, 0)
	case Day:
		return start.AddDate(0, 0, 1)
	default:
		return start.Add(time.Hour)
	}
}

// passed is how much of [from, to) lies before now.
func passed(from, to, now time.Time) time.Duration {
	if now.Before(to) {
		to = now
	}
	if !to.After(from) {
		return 0
	}
	return to.Sub(from)
}

// BuildReport assembles the report of q from what a Reader.Usage returned.
func BuildReport(q Query, d Data, now time.Time) Report {
	r := Report{From: q.From, To: q.To, Granularity: q.Granularity, Periods: []Period{}, TopDevices: []ClientTransfer{}}
	index := map[time.Time]int{}
	period := func(t time.Time) *Period {
		start := periodStart(t, q.Granularity)
		i, ok := index[start]
		if !ok {
			i = len(r.Periods)
			index[start] = i
			r.Periods = append(r.Periods, Period{Start: start})
		}
		return &r.Periods[i]
	}
	for _, row := range d.Periods {
		period(row.Period).add(row.Metric, row.Value)
		r.Totals.add(row.Metric, row.Value)
	}
	days := q.Granularity != Hour
	for _, dc := range d.Days {
		if days {
			period(dc.Day).addDay(dc.Devices)
		}
		r.Totals.addDay(dc.Devices)
	}
	for i := range r.Periods {
		p := &r.Periods[i]
		from, to := p.Start, periodEnd(p.Start, q.Granularity)
		if from.Before(q.From) {
			from = q.From
		}
		if to.After(q.To) {
			to = q.To
		}
		p.finish(passed(from, to, now), days)
	}
	sort.Slice(r.Periods, func(i, j int) bool { return r.Periods[i].Start.Before(r.Periods[j].Start) })
	r.Totals.finish(passed(q.From, q.To, now), true)
	r.TopDevices = topDevices(d.Top)
	return r
}

func topDevices(rows []ClientRow) []ClientTransfer {
	by := map[string]*ClientTransfer{}
	out := []ClientTransfer{}
	for _, row := range rows {
		c := by[row.Client]
		if c == nil {
			c = &ClientTransfer{ClientID: row.Client}
			by[row.Client] = c
		}
		switch Metric(row.Metric) {
		case TunnelBytesToUser:
			c.TunnelBytesToUser += row.Value
		case TunnelBytesToDevice:
			c.TunnelBytesToDevice += row.Value
		case UploadBytesToDevices:
			c.UploadBytesToDevices += row.Value
		default:
			continue
		}
		c.TransferBytes += row.Value
	}
	for _, c := range by {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TransferBytes != out[j].TransferBytes {
			return out[i].TransferBytes > out[j].TransferBytes
		}
		return out[i].ClientID < out[j].ClientID
	})
	if len(out) > TopClients {
		out = out[:TopClients]
	}
	return out
}

// TenantTotals returns the totals of each tenant over [from, to).
func TenantTotals(rows []TenantRow, days []TenantDays, from, to, now time.Time) map[string]Totals {
	out := map[string]Totals{}
	for _, row := range rows {
		t := out[row.Tenant]
		t.add(row.Metric, row.Value)
		out[row.Tenant] = t
	}
	for _, d := range days {
		t := out[d.Tenant]
		t.DeviceDays, t.DevicesPeakDay = d.DeviceDays, d.PeakDay
		out[d.Tenant] = t
	}
	span := passed(from, to, now)
	for id, t := range out {
		t.finish(span, true)
		out[id] = t
	}
	return out
}
