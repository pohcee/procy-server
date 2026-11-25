package metering

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseQuery(t *testing.T) {
	now := time.Date(2026, 11, 24, 10, 30, 0, 0, time.UTC)
	q, err := ParseQuery("", "", "", now)
	require.NoError(t, err)
	assert.Equal(t, Query{From: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC), Granularity: Day}, q,
		"the month so far, in whole days")

	q, err = ParseQuery("2026-11-24T08:15:00Z", "2026-11-24T10:30:00+00:00", "hour", now)
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 11, 24, 8, 0, 0, 0, time.UTC), q.From)
	assert.Equal(t, time.Date(2026, 11, 24, 11, 0, 0, 0, time.UTC), q.To)

	for _, c := range [][3]string{
		{"", "", "week"},
		{"yesterday", "", "day"},
		{"2026-11-24", "2026-11-20", "day"},
		{"2026-01-01", "2026-11-24", "hour"}, // more than 31 days by hour
	} {
		_, err := ParseQuery(c[0], c[1], c[2], now)
		assert.True(t, errors.Is(err, ErrInvalid), "%v: %v", c, err)
	}

	q, err = ParseQuery("", "2026-12-01", "day", now)
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), q.From, "to is excluded: the month before")

	from, to, err := ParseMonth("2026-02", now)
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), from)
	assert.Equal(t, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), to)
	_, _, err = ParseMonth("02/2026", now)
	assert.ErrorIs(t, err, ErrInvalid)
}

func TestBuildReport(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	q, err := ParseQuery("2026-11-01", "", "day", now)
	require.NoError(t, err)
	d1, d2 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	r := BuildReport(q, Data{
		Periods: []PeriodRow{
			{Period: d2, Metric: "tunnel_bytes_to_user", Value: 300},
			{Period: d1, Metric: "tunnel_bytes_to_user", Value: 100},
			{Period: d1, Metric: "upload_bytes_to_devices", Value: 50},
			{Period: d1, Metric: "jobs_command", Value: 2},
			{Period: d1, Metric: "jobs_library", Value: 1},
			{Period: d1, Metric: "device_seconds", Value: 2 * 86400},
			{Period: d1, Metric: "devices_peak", Value: 3},
			{Period: d2, Metric: "devices_peak", Value: 5},
			{Period: d1, Metric: "unknown_metric", Value: 9},
		},
		Days: []DayCount{{Day: d1, Devices: 3}, {Day: d2, Devices: 4}},
		Top: []ClientRow{
			{Client: "c1", Metric: "tunnel_bytes_to_user", Value: 10},
			{Client: "c2", Metric: "tunnel_bytes_to_user", Value: 300},
			{Client: "c2", Metric: "upload_bytes_to_devices", Value: 5},
		},
	}, now)
	require.Len(t, r.Periods, 2)
	assert.Equal(t, d1, r.Periods[0].Start)
	assert.EqualValues(t, 150, r.Periods[0].TransferBytes)
	assert.EqualValues(t, 3, r.Periods[0].JobsRun)
	assert.EqualValues(t, 48, r.Periods[0].DeviceHours)
	assert.EqualValues(t, 2, r.Periods[0].DevicesAvg)
	assert.EqualValues(t, 3, r.Periods[0].DeviceDays)
	assert.EqualValues(t, 3, r.Periods[0].DevicesBillable)
	assert.EqualValues(t, 400, r.Totals.TunnelBytesToUser)
	assert.EqualValues(t, 5, r.Totals.DevicesPeak)
	assert.EqualValues(t, 7, r.Totals.DeviceDays)
	assert.EqualValues(t, 4, r.Totals.DevicesPeakDay)
	// 7 device-days over the 3 days begun (Nov 1, 2 and half of 3): 2.33, rounded up
	assert.EqualValues(t, 3, r.Totals.DevicesBillable)
	require.Len(t, r.TopDevices, 2)
	assert.Equal(t, "c2", r.TopDevices[0].ClientID)
	assert.EqualValues(t, 305, r.TopDevices[0].TransferBytes)
}

func TestTenantTotals(t *testing.T) {
	from, to, _ := ParseMonth("2026-11", time.Time{})
	now := time.Date(2026, 12, 5, 0, 0, 0, 0, time.UTC)
	tt := TenantTotals([]TenantRow{
		{Tenant: "a", Metric: "tunnel_bytes_to_device", Value: 7},
		{Tenant: "b", Metric: "jobs_script", Value: 2},
	}, []TenantDays{{Tenant: "a", DeviceDays: 300, PeakDay: 12}}, from, to, now)
	assert.EqualValues(t, 7, tt["a"].TransferBytes)
	assert.EqualValues(t, 10, tt["a"].DevicesBillable, "300 device-days over 30 days")
	assert.EqualValues(t, 12, tt["a"].DevicesPeakDay)
	assert.EqualValues(t, 2, tt["b"].JobsRun)
}
