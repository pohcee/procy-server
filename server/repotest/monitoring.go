package repotest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/monitoring"
	"github.com/pohcee/procy-server/share/models"
	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/tenant"
	"github.com/pohcee/procy-server/share/types"
)

// monitoringLayout is how the monitoring API passes times to the provider.
const monitoringLayout = "2006-01-02 15:04:05"

var (
	monInterval = time.Minute
	mon1        = time.Date(2021, time.September, 1, 0, 0, 0, 0, time.UTC)
	mon2        = mon1.Add(monInterval)
	mon3        = mon2.Add(monInterval)
)

func monTestData() []*models.Measurement {
	return []*models.Measurement{
		{
			ClientID: "test_client_1", Timestamp: mon1, CPUUsagePercent: 10, MemoryUsagePercent: 30, IoUsagePercent: 2,
			Processes:   `{[{"pid":30210, "parent_pid": 4711, "name": "chrome"}]}`,
			Mountpoints: `{"free_b./":34182758400,"free_b./home":128029413376,"total_b./":105555197952,"total_b./home":364015185920}`,
			NetLan:      &models.NetBytes{In: 3000, Out: 2000},
		},
		{
			ClientID: "test_client_1", Timestamp: mon2, CPUUsagePercent: 15, MemoryUsagePercent: 35, IoUsagePercent: 3,
			Processes:   `{[{"pid":30211, "parent_pid": 4711, "name": "idea"}]}`,
			Mountpoints: `{"free_b./":44182758400,"free_b./home":228029413376,"total_b./":105555197952,"total_b./home":364015185920}`,
			NetLan:      &models.NetBytes{In: 3300, Out: 2200},
		},
		{
			ClientID: "test_client_1", Timestamp: mon3, CPUUsagePercent: 20, MemoryUsagePercent: 40, IoUsagePercent: 4,
			Processes:   `{[{"pid":30212, "parent_pid": 4711, "name": "cinnamon"}]}`,
			Mountpoints: `{"free_b./":54182758400,"free_b./home":328029413376,"total_b./":105555197952,"total_b./home":364015185920}`,
			NetLan:      &models.NetBytes{In: 3330, Out: 2220},
		},
	}
}

func monCreateTestData(ctx context.Context, t *testing.T, p monitoring.DBProvider) {
	t.Helper()
	for _, m := range monTestData() {
		require.NoError(t, p.CreateMeasurement(ctx, m))
	}
}

// monCreateDownsamplingData stores 48 hours of measurements, one a minute, alternating between low and high values.
func monCreateDownsamplingData(ctx context.Context, t *testing.T, p monitoring.DBProvider) {
	t.Helper()
	for i := 0; i < 60*48; i++ {
		r := float64(i % 2)
		require.NoError(t, p.CreateMeasurement(ctx, &models.Measurement{
			ClientID:           "test_client",
			Timestamp:          mon1.Add(time.Duration(i) * monInterval),
			CPUUsagePercent:    10.0 + r*10,
			MemoryUsagePercent: 10.0 + r*10,
			IoUsagePercent:     10.0 + r*10,
			NetLan:             &models.NetBytes{In: 10000 + i*10, Out: 50000 + i*10},
		}))
	}
}

func monListOptions(fields map[string][]string, limit string) *query.ListOptions {
	return &query.ListOptions{
		Sorts:      query.ParseSortOptions(map[string][]string{"sort": {"-timestamp"}}),
		Fields:     query.ParseFieldsOptions(fields),
		Pagination: &query.Pagination{Limit: limit, Offset: "0"},
	}
}

func monMetricsOptions() *query.ListOptions {
	return monListOptions(monitoring.ClientMetricsFieldsDefault, "1")
}

func monProcessesOptions() *query.ListOptions {
	return monListOptions(monitoring.ClientProcessesFieldsDefault, "1")
}

func monMountpointsOptions() *query.ListOptions {
	return monListOptions(monitoring.ClientMountpointsFieldsDefault, "1")
}

func monTimeFilters(lower, upper query.FilterOperatorType, start time.Time, hours float64) []query.FilterOption {
	return []query.FilterOption{
		{Column: []string{"timestamp"}, Operator: lower, Values: []string{start.Format(monitoringLayout)}},
		{Column: []string{"timestamp"}, Operator: upper, Values: []string{start.Add(time.Duration(hours * float64(time.Hour))).Format(monitoringLayout)}},
	}
}

func monGraphOptions(start time.Time, hours float64) *query.ListOptions {
	return &query.ListOptions{
		Sorts:   query.ParseSortOptions(monitoring.ClientGraphMetricsSortDefault),
		Filters: monTimeFilters(query.FilterOperatorTypeSince, query.FilterOperatorTypeUntil, start, hours),
		Fields:  query.ParseFieldsOptions(monitoring.ClientGraphMetricsFieldsDefault),
	}
}

func monMeasurement(clientID string, ts time.Time, cpu float64) *models.Measurement {
	return &models.Measurement{ClientID: clientID, Timestamp: ts, CPUUsagePercent: cpu, Processes: "[]", Mountpoints: "{}"}
}

// Monitoring runs the contract tests of monitoring.DBProvider.
func Monitoring(t *testing.T, newProvider Factory[monitoring.DBProvider]) {
	a, b := Ctx(TenantA), Ctx(TenantB)

	t.Run("RequiresTenant", func(t *testing.T) {
		p := newProvider(t)
		ctx := context.Background()

		assert.ErrorIs(t, p.CreateMeasurement(ctx, monMeasurement("c1", mon1, 1)), tenant.ErrMissing)
		_, err := p.DeleteMeasurementsBefore(ctx, mon3)
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = p.CountByClientID(ctx, "c1", monMetricsOptions())
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = p.ListMetricsByClientID(ctx, "c1", monMetricsOptions())
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = p.ListProcessesByClientID(ctx, "c1", monProcessesOptions())
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = p.ListMountpointsByClientID(ctx, "c1", monMountpointsOptions())
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = p.ListGraphMetricsByClientID(ctx, "c1", 2, monGraphOptions(mon1, 2))
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = p.ListGraphByClientID(ctx, "c1", 2, monGraphOptions(mon1, 2), "cpu_usage_percent")
		assert.ErrorIs(t, err, tenant.ErrMissing)
	})

	t.Run("Metrics", func(t *testing.T) {
		p := newProvider(t)
		monCreateTestData(a, t, p)

		count, err := p.CountByClientID(a, "test_client_1", monMetricsOptions())
		require.NoError(t, err)
		assert.Equal(t, 3, count, "count ignores pagination")

		latest, err := p.ListMetricsByClientID(a, "test_client_1", monMetricsOptions())
		require.NoError(t, err)
		require.Len(t, latest, 1)
		assert.True(t, mon3.Equal(latest[0].Timestamp), latest[0].Timestamp)
		assert.Equal(t, 20.0, latest[0].CPUUsagePercent)
		assert.Equal(t, 40.0, latest[0].MemoryUsagePercent)
		assert.Equal(t, 4.0, latest[0].IOUsagePercent)

		next := monMetricsOptions()
		next.Pagination.Offset = "1"
		second, err := p.ListMetricsByClientID(a, "test_client_1", next)
		require.NoError(t, err)
		require.Len(t, second, 1)
		assert.True(t, mon2.Equal(second[0].Timestamp), second[0].Timestamp)

		asc := monListOptions(monitoring.ClientMetricsFieldsDefault, "10")
		asc.Sorts = []query.SortOption{{Column: "timestamp", IsASC: true}}
		all, err := p.ListMetricsByClientID(a, "test_client_1", asc)
		require.NoError(t, err)
		require.Len(t, all, 3)
		assert.Equal(t, []float64{10, 15, 20}, []float64{all[0].CPUUsagePercent, all[1].CPUUsagePercent, all[2].CPUUsagePercent})

		// the API's time filters
		since := monListOptions(monitoring.ClientMetricsFieldsDefault, "10")
		since.Filters = monTimeFilters(query.FilterOperatorTypeSince, query.FilterOperatorTypeUntil, mon2, 1)
		inRange, err := p.ListMetricsByClientID(a, "test_client_1", since)
		require.NoError(t, err)
		assert.Len(t, inRange, 2)
		count, err = p.CountByClientID(a, "test_client_1", since)
		require.NoError(t, err)
		assert.Equal(t, 2, count)
		gtlt := monListOptions(monitoring.ClientMetricsFieldsDefault, "10")
		gtlt.Filters = monTimeFilters(query.FilterOperatorTypeGT, query.FilterOperatorTypeLT, mon1.Add(30*time.Second), 1.0/60)
		between, err := p.ListMetricsByClientID(a, "test_client_1", gtlt)
		require.NoError(t, err)
		require.Len(t, between, 1)
		assert.True(t, mon2.Equal(between[0].Timestamp))

		unknown, err := p.ListMetricsByClientID(a, "nope", monMetricsOptions())
		require.NoError(t, err)
		assert.Empty(t, unknown)
		count, err = p.CountByClientID(a, "nope", monMetricsOptions())
		require.NoError(t, err)
		assert.Zero(t, count)
	})

	t.Run("Processes", func(t *testing.T) {
		p := newProvider(t)
		monCreateTestData(a, t, p)

		latest, err := p.ListProcessesByClientID(a, "test_client_1", monProcessesOptions())
		require.NoError(t, err)
		require.Len(t, latest, 1)
		assert.Equal(t, types.JSONString(`{[{"pid":30212, "parent_pid": 4711, "name": "cinnamon"}]}`), latest[0].Processes)
		assert.True(t, mon3.Equal(latest[0].Timestamp))

		third := monProcessesOptions()
		third.Pagination.Offset = "2"
		oldest, err := p.ListProcessesByClientID(a, "test_client_1", third)
		require.NoError(t, err)
		require.Len(t, oldest, 1)
		assert.Equal(t, types.JSONString(`{[{"pid":30210, "parent_pid": 4711, "name": "chrome"}]}`), oldest[0].Processes)
	})

	t.Run("Mountpoints", func(t *testing.T) {
		p := newProvider(t)
		monCreateTestData(a, t, p)

		latest, err := p.ListMountpointsByClientID(a, "test_client_1", monMountpointsOptions())
		require.NoError(t, err)
		require.Len(t, latest, 1)
		assert.Equal(t, types.JSONString(`{"free_b./":54182758400,"free_b./home":328029413376,"total_b./":105555197952,"total_b./home":364015185920}`), latest[0].Mountpoints)

		two := monMountpointsOptions()
		two.Pagination.Limit = "2"
		page, err := p.ListMountpointsByClientID(a, "test_client_1", two)
		require.NoError(t, err)
		require.Len(t, page, 2)
		assert.Equal(t, types.JSONString(`{"free_b./":54182758400,"free_b./home":328029413376,"total_b./":105555197952,"total_b./home":364015185920}`), page[0].Mountpoints)
		assert.Equal(t, types.JSONString(`{"free_b./":44182758400,"free_b./home":228029413376,"total_b./":105555197952,"total_b./home":364015185920}`), page[1].Mountpoints)
	})

	t.Run("DuplicateMeasurement", func(t *testing.T) {
		p := newProvider(t)
		require.NoError(t, p.CreateMeasurement(a, monMeasurement("c1", mon1, 1)))
		assert.Error(t, p.CreateMeasurement(a, monMeasurement("c1", mon1, 2)), "one measurement per client and time")
		require.NoError(t, p.CreateMeasurement(a, monMeasurement("c2", mon1, 3)))
	})

	t.Run("Graphs", func(t *testing.T) {
		p := newProvider(t)
		monCreateDownsamplingData(a, t, p)
		hours := 48.0

		points, err := p.ListGraphMetricsByClientID(a, "test_client", hours, monGraphOptions(mon1, hours))
		require.NoError(t, err)
		// 48 hours, downsampled into buckets of 48*29 seconds
		require.Len(t, points, 126)
		for i, pt := range points {
			if i > 0 {
				assert.True(t, pt.Timestamp.Before(points[i-1].Timestamp), "newest first")
			}
			assert.False(t, pt.Timestamp.Before(mon1))
			assert.Equal(t, 10.0, pt.CPUUsagePercent.Min)
			assert.Equal(t, 20.0, pt.CPUUsagePercent.Max)
			assert.InDelta(t, 15.0, pt.CPUUsagePercent.Avg, 1)
			assert.Equal(t, 10.0, pt.MemoryUsagePercent.Min)
			assert.Equal(t, 20.0, pt.IOUsagePercent.Max)
		}
		// averages are rounded to two decimals
		for _, pt := range points {
			assert.InDelta(t, pt.CPUUsagePercent.Avg, float64(int(pt.CPUUsagePercent.Avg*100+0.5))/100, 1e-9)
		}

		gtlt := monGraphOptions(mon1, hours)
		gtlt.Filters = monTimeFilters(query.FilterOperatorTypeGT, query.FilterOperatorTypeLT, mon1, hours)
		points, err = p.ListGraphMetricsByClientID(a, "test_client", hours, gtlt)
		require.NoError(t, err)
		require.Len(t, points, 126)

		// two hours: buckets of 58 seconds hold one measurement each
		short, err := p.ListGraphMetricsByClientID(a, "test_client", 2, monGraphOptions(mon1.Add(30*time.Second), 2))
		require.NoError(t, err)
		assert.Len(t, short, 120)
		for _, pt := range short {
			assert.Equal(t, pt.CPUUsagePercent.Min, pt.CPUUsagePercent.Max)
			assert.Equal(t, pt.CPUUsagePercent.Min, pt.CPUUsagePercent.Avg)
		}

		for graph := range monitoring.ClientGraphNameToField {
			g, err := p.ListGraphByClientID(a, "test_client", hours, monGraphOptions(mon1, hours), graph)
			require.NoError(t, err, graph)
			require.Len(t, g, 126, graph)
			first := g[0]
			switch graph {
			case "cpu_usage_percent":
				require.NotNil(t, first.CPUUsagePercent)
				assert.Equal(t, 20.0, first.CPUUsagePercent.Max)
				assert.Nil(t, first.MemoryUsagePercent)
			case "mem_usage_percent":
				require.NotNil(t, first.MemoryUsagePercent)
				assert.Equal(t, 10.0, first.MemoryUsagePercent.Min)
			case "io_usage_percent":
				require.NotNil(t, first.IOUsagePercent)
				assert.Equal(t, 20.0, first.IOUsagePercent.Max)
			case "net_usage_bps_lan", "net_usage_percent_lan":
				var in, out *float64
				if graph == "net_usage_bps_lan" {
					require.NotNil(t, first.NetUsageBPSLan)
					in, out = first.NetUsageBPSLan.InMax, first.NetUsageBPSLan.OutMax
				} else {
					require.NotNil(t, first.NetUsagePercentLan)
					in, out = first.NetUsagePercentLan.InMax, first.NetUsagePercentLan.OutMax
				}
				require.NotNil(t, in)
				require.NotNil(t, out)
				assert.Equal(t, float64(10000+2879*10), *in, "the newest bucket holds the last measurement")
				assert.Equal(t, float64(50000+2879*10), *out)
			case "net_usage_bps_wan", "net_usage_percent_wan":
				// no WAN data was measured
				if first.NetUsageBPSWan != nil {
					assert.Nil(t, first.NetUsageBPSWan.InAvg)
					assert.Nil(t, first.NetUsageBPSWan.OutMax)
				}
				if first.NetUsagePercentWan != nil {
					assert.Nil(t, first.NetUsagePercentWan.InAvg)
				}
			}
		}

		_, err = p.ListGraphByClientID(a, "test_client", hours, monGraphOptions(mon1, hours), "illegal_graph_name")
		assert.Error(t, err)

		none, err := p.ListGraphMetricsByClientID(b, "test_client", hours, monGraphOptions(mon1, hours))
		require.NoError(t, err)
		assert.Empty(t, none)
	})

	t.Run("DeleteMeasurementsBefore", func(t *testing.T) {
		p := newProvider(t)
		monCreateTestData(a, t, p)
		require.NoError(t, p.CreateMeasurement(a, monMeasurement("other_client", mon1, 1)))

		// every client's measurements before the time go
		deleted, err := p.DeleteMeasurementsBefore(a, mon3)
		require.NoError(t, err)
		assert.Equal(t, int64(3), deleted)
		count, err := p.CountByClientID(a, "test_client_1", monMetricsOptions())
		require.NoError(t, err)
		assert.Equal(t, 1, count)
		count, err = p.CountByClientID(a, "other_client", monMetricsOptions())
		require.NoError(t, err)
		assert.Zero(t, count)

		deleted, err = p.DeleteMeasurementsBefore(a, mon3)
		require.NoError(t, err)
		assert.Zero(t, deleted)
	})

	t.Run("IsolatesTenants", func(t *testing.T) {
		p := newProvider(t)
		// the same client ID in both tenants (at different times: a store may key measurements by client and time
		// only)
		require.NoError(t, p.CreateMeasurement(a, monMeasurement("c1", mon1, 10)))
		require.NoError(t, p.CreateMeasurement(a, monMeasurement("c1", mon2, 11)))
		require.NoError(t, p.CreateMeasurement(b, monMeasurement("c1", mon1.Add(time.Second), 20)))

		count, err := p.CountByClientID(a, "c1", monMetricsOptions())
		require.NoError(t, err)
		assert.Equal(t, 2, count)
		count, err = p.CountByClientID(b, "c1", monMetricsOptions())
		require.NoError(t, err)
		assert.Equal(t, 1, count)

		metrics, err := p.ListMetricsByClientID(b, "c1", monMetricsOptions())
		require.NoError(t, err)
		require.Len(t, metrics, 1)
		assert.Equal(t, 20.0, metrics[0].CPUUsagePercent)

		procs, err := p.ListProcessesByClientID(b, "c1", &query.ListOptions{Fields: query.ParseFieldsOptions(monitoring.ClientProcessesFieldsDefault)})
		require.NoError(t, err)
		assert.Len(t, procs, 1)
		mps, err := p.ListMountpointsByClientID(b, "c1", &query.ListOptions{Fields: query.ParseFieldsOptions(monitoring.ClientMountpointsFieldsDefault)})
		require.NoError(t, err)
		assert.Len(t, mps, 1)

		graph, err := p.ListGraphMetricsByClientID(b, "c1", 2, monGraphOptions(mon1.Add(-time.Minute), 2))
		require.NoError(t, err)
		require.Len(t, graph, 1)
		assert.Equal(t, 20.0, graph[0].CPUUsagePercent.Avg)

		g, err := p.ListGraphByClientID(b, "c1", 2, monGraphOptions(mon1.Add(-time.Minute), 2), "cpu_usage_percent")
		require.NoError(t, err)
		require.Len(t, g, 1)
		assert.Equal(t, 20.0, g[0].CPUUsagePercent.Avg)

		// deleting in tenant a leaves tenant b alone
		deleted, err := p.DeleteMeasurementsBefore(a, mon3)
		require.NoError(t, err)
		assert.Equal(t, int64(2), deleted)
		count, err = p.CountByClientID(b, "c1", monMetricsOptions())
		require.NoError(t, err)
		assert.Equal(t, 1, count)
		count, err = p.CountByClientID(Ctx("c"), "c1", monMetricsOptions())
		require.NoError(t, err)
		assert.Zero(t, count)
	})
}
