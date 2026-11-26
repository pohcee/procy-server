package usage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/metering"
	"github.com/pohcee/procy-server/server/repotest"
	"github.com/pohcee/procy-server/server/storage/outbox"
	"github.com/pohcee/procy-server/server/storage/postgres/device"
	"github.com/pohcee/procy-server/server/storage/postgres/pgtest"
	"github.com/pohcee/procy-server/share/tenant"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var ctx = context.Background()

func thisHour() time.Time { return time.Now().UTC().Truncate(time.Hour) }

func today() time.Time {
	n := time.Now().UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
}

func addClient(t *testing.T, d *pgtest.DB, tid tenant.ID, id string) {
	t.Helper()
	_, err := d.Owner.Exec("INSERT INTO clients (tenant_id, id, client_auth_id, details) VALUES ($1, $2, 'auth', '{}')", string(tid), id)
	require.NoError(t, err)
}

func hourly(t *testing.T, d *pgtest.DB) map[string]int64 {
	t.Helper()
	var rows []struct {
		Tenant string    `db:"tenant_id"`
		Hour   time.Time `db:"hour"`
		Metric string    `db:"metric"`
		Value  int64     `db:"value"`
	}
	require.NoError(t, d.Owner.Select(&rows, "SELECT tenant_id, hour, metric, value FROM usage_hourly"))
	out := map[string]int64{}
	for _, r := range rows {
		out[r.Tenant+"/"+r.Metric] += r.Value
	}
	return out
}

func rec(tid tenant.ID, key string, deltas ...metering.Delta) metering.Record {
	return metering.Record{Tenant: tid, Node: "n1", Key: key, Deltas: deltas}
}

func TestIngestAddsOnceAndKeepsGaugePeaks(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	addClient(t, d, repotest.TenantA, "c1")
	w := Writer{DB: d.App}
	h := thisHour()
	r1 := rec(repotest.TenantA, "k1",
		metering.Delta{Hour: h, Metric: metering.TunnelBytesToUser, Client: "c1", Value: 1000},
		metering.Delta{Hour: h, Metric: metering.TunnelBytesToUser, Client: "unknown", Value: 5},
		metering.Delta{Hour: h, Metric: metering.DevicesPeak, Value: 4})
	r1.Devices = []metering.DeviceDay{{Day: today(), Clients: []string{"c1", "not-a-client"}}}
	require.NoError(t, w.WriteUsage(ctx, []metering.Record{r1}))
	// replayed (the flush is retried after a lost answer): nothing is added again
	require.NoError(t, w.WriteUsage(ctx, []metering.Record{r1}))
	r2 := rec(repotest.TenantA, "k2",
		metering.Delta{Hour: h, Metric: metering.TunnelBytesToUser, Client: "c1", Value: 1},
		metering.Delta{Hour: h, Metric: metering.DevicesPeak, Value: 2})
	r2.Devices = []metering.DeviceDay{{Day: today(), Clients: []string{"c1"}}}
	require.NoError(t, w.WriteUsage(ctx, []metering.Record{r2, rec(repotest.TenantB, "k1", metering.Delta{Hour: h, Metric: metering.JobsCommand, Value: 3})}))

	got := hourly(t, d)
	assert.EqualValues(t, 1006, got["a/tunnel_bytes_to_user"], "unknown clients still count for the tenant")
	assert.EqualValues(t, 4, got["a/devices_peak"], "a gauge keeps the highest value")
	assert.EqualValues(t, 3, got["b/jobs_command"], "keys are per tenant")

	var client int64
	require.NoError(t, d.Owner.Get(&client, "SELECT sum(value) FROM usage_client_daily WHERE tenant_id = 'a' AND client_id = 'c1'"))
	assert.EqualValues(t, 1001, client)
	var n int
	require.NoError(t, d.Owner.Get(&n, "SELECT count(*) FROM usage_client_daily WHERE client_id = 'unknown'"))
	assert.Zero(t, n)
	var days []string
	require.NoError(t, d.Owner.Select(&days, "SELECT client_id FROM usage_device_days"))
	assert.Equal(t, []string{"c1"}, days, "a device counts once a day, unknown ids are skipped")
}

func TestIngestRejectsInvalidItems(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	_, err := d.Owner.Exec("INSERT INTO tenants (id, name, status) VALUES ('_r', 'r', 'reserved'), ('s', 's', 'suspended')")
	require.NoError(t, err)
	h := thisHour()
	sink := device.Sink{DB: d.App}
	items := []map[string]any{
		{"tenant_id": "a", "key": "bad-hour", "deltas": []any{map[string]any{"hour": h.Add(time.Minute).Format(time.RFC3339), "metric": "jobs_command", "value": 1}}},
		{"tenant_id": "a", "key": "future", "deltas": []any{map[string]any{"hour": h.Add(3 * time.Hour).Format(time.RFC3339), "metric": "jobs_command", "value": 1}}},
		{"tenant_id": "a", "key": "old", "deltas": []any{map[string]any{"hour": h.Add(-8 * 24 * time.Hour).Format(time.RFC3339), "metric": "jobs_command", "value": 1}}},
		{"tenant_id": "a", "key": "metric", "deltas": []any{map[string]any{"hour": h.Format(time.RFC3339), "metric": "monitoring_rows", "value": 1}}},
		{"tenant_id": "a", "key": "big", "deltas": []any{map[string]any{"hour": h.Format(time.RFC3339), "metric": "jobs_command", "value": 2000000}}},
		{"tenant_id": "a", "key": "neg", "deltas": []any{map[string]any{"hour": h.Format(time.RFC3339), "metric": "jobs_command", "value": -1}}},
		{"tenant_id": "a", "key": "day", "devices": []any{map[string]any{"day": "2020-01-01", "client_ids": []string{"x"}}}},
		{"tenant_id": "_r", "key": "reserved"},
		{"tenant_id": "nope", "key": "unknown"},
		// the first delta is fine, the second not: nothing of the item is kept
		{"tenant_id": "a", "key": "partial", "deltas": []any{
			map[string]any{"hour": h.Format(time.RFC3339), "metric": "jobs_script", "value": 1},
			map[string]any{"hour": h.Format(time.RFC3339), "metric": "jobs_script", "value": "x"}}},
		{"tenant_id": "s", "key": "suspended-ok", "deltas": []any{map[string]any{"hour": h.Format(time.RFC3339), "metric": "jobs_command", "value": 1}}},
	}
	var obItems []outbox.Item
	for _, it := range items {
		b, err := json.Marshal(it)
		require.NoError(t, err)
		obItems = append(obItems, outbox.Item{Key: it["key"].(string), Payload: b})
	}
	st, err := sink.Ship(ctx, outbox.KindUsage, obItems)
	require.NoError(t, err)
	require.Len(t, st, len(items))
	for _, s := range st {
		if s.Key == "suspended-ok" {
			assert.Equal(t, "applied", s.Outcome)
			continue
		}
		assert.True(t, strings.HasPrefix(s.Outcome, "rejected"), "%s: %s", s.Key, s.Outcome)
	}
	assert.Equal(t, map[string]int64{"s/jobs_command": 1}, hourly(t, d))
	var keys int
	require.NoError(t, d.Owner.Get(&keys, "SELECT count(*) FROM ingest_keys WHERE kind = 'usage' AND tenant_id = 'a'"))
	assert.Zero(t, keys, "a rejected item's claim is rolled back")
}

// A device process writes usage into its outbox; the Shipper ships it through ingest_usage with the device role.
func TestDeviceProcessShipsUsageThroughTheOutbox(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	ob, err := outbox.Open(filepath.Join(t.TempDir(), "outbox.db"), outbox.Options{MaxPerTenant: 2})
	require.NoError(t, err)
	defer ob.Close()
	// a full tenant still takes usage: it is never dropped
	for i, k := range []string{"j1", "j2"} {
		require.NoError(t, ob.Add(ctx, outbox.KindJobResult, repotest.TenantA, k, []byte(`{}`)), i)
	}
	m := metering.New(metering.Options{Node: "dev1", Writer: device.Usage{Outbox: ob}})
	m.Add(repotest.TenantA, "", metering.JobsLibrary, 2)
	require.NoError(t, m.Flush(ctx))
	n, err := ob.Count(ctx, repotest.TenantA)
	require.NoError(t, err)
	assert.Equal(t, 3, n, "usage is kept over the cap")

	sh := &outbox.Shipper{Outbox: ob, Sink: device.Sink{DB: d.Device}}
	_, err = sh.ShipAll(ctx)
	require.NoError(t, err) // the empty job results are rejected (dead letters), usage ships
	assert.EqualValues(t, 2, hourly(t, d)["a/jobs_library"])
	left, err := ob.Pending(ctx, outbox.KindUsage, 10)
	require.NoError(t, err)
	assert.Empty(t, left)
}

func TestStoreReadsAndIsolatesTenants(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	addClient(t, d, repotest.TenantA, "c1")
	addClient(t, d, repotest.TenantA, "c2")
	h := thisHour()
	w := Writer{DB: d.App}
	ra := rec(repotest.TenantA, "k",
		metering.Delta{Hour: h, Metric: metering.TunnelBytesToUser, Client: "c1", Value: 10},
		metering.Delta{Hour: h, Metric: metering.TunnelBytesToUser, Client: "c2", Value: 30},
		metering.Delta{Hour: h.Add(-time.Hour), Metric: metering.UploadBytesToDevices, Client: "c1", Value: 5})
	ra.Devices = []metering.DeviceDay{{Day: today(), Clients: []string{"c1", "c2"}}}
	require.NoError(t, w.WriteUsage(ctx, []metering.Record{ra,
		rec(repotest.TenantB, "k", metering.Delta{Hour: h, Metric: metering.TunnelBytesToUser, Value: 999})}))

	s := Store{DB: d.App}
	now := time.Now()
	q, err := metering.ParseQuery(h.Add(-2*time.Hour).Format(time.RFC3339), "", "hour", now)
	require.NoError(t, err)
	data, err := s.Usage(ctx, repotest.TenantA, q)
	require.NoError(t, err)
	r := metering.BuildReport(q, data, now)
	assert.EqualValues(t, 40, r.Totals.TunnelBytesToUser, "tenant b's 999 bytes are not in a's report")
	assert.EqualValues(t, 45, r.Totals.TransferBytes)
	assert.Len(t, r.Periods, 2)
	assert.EqualValues(t, 2, r.Totals.DeviceDays)
	require.Len(t, r.TopDevices, 2)
	assert.Equal(t, "c2", r.TopDevices[0].ClientID)

	q, err = metering.ParseQuery("", "", "day", now)
	require.NoError(t, err)
	data, err = s.Usage(ctx, repotest.TenantB, q)
	require.NoError(t, err)
	r = metering.BuildReport(q, data, now)
	assert.EqualValues(t, 999, r.Totals.TunnelBytesToUser)
	assert.Empty(t, r.TopDevices)

	rows, days, err := s.Summary(ctx, h.Add(-24*time.Hour), h.Add(time.Hour))
	require.NoError(t, err)
	tt := metering.TenantTotals(rows, days, h.Add(-24*time.Hour), h.Add(time.Hour), now)
	assert.EqualValues(t, 45, tt["a"].TransferBytes)
	assert.EqualValues(t, 999, tt["b"].TransferBytes)
	assert.EqualValues(t, 2, tt["a"].DeviceDays)
}

func TestUpkeepRollsUpRecordsMonitoringAndPrunes(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	addClient(t, d, repotest.TenantA, "c1")
	old := today().AddDate(0, 0, -5).Add(3 * time.Hour)
	ancient := today().AddDate(0, 0, -100)
	_, err := d.Owner.Exec(`INSERT INTO usage_hourly (tenant_id, hour, metric, value) VALUES
		('a', $1, 'tunnel_bytes_to_user', 10), ('a', $1 + interval '1 hour', 'tunnel_bytes_to_user', 5),
		('a', $1, 'devices_peak', 3), ('a', $1 + interval '1 hour', 'devices_peak', 7),
		('a', $2, 'jobs_command', 1)`, old, ancient)
	require.NoError(t, err)
	_, err = d.Owner.Exec(`INSERT INTO usage_device_days (tenant_id, day, client_id) VALUES ('a', $1, 'c1')`, today().AddDate(-3, 0, 0))
	require.NoError(t, err)
	_, err = d.Owner.Exec(`INSERT INTO measurements (tenant_id, client_id, timestamp, cpu_usage_percent, memory_usage_percent, io_usage_percent)
		VALUES ('a', 'c1', now(), 1, 1, 1), ('a', 'c1', now() - interval '1 minute', 1, 1, 1)`)
	require.NoError(t, err)

	require.NoError(t, Upkeep(ctx, d.App, Retention{Monitoring: true}))
	require.NoError(t, Upkeep(ctx, d.App, Retention{Monitoring: true}), "twice changes nothing")

	var daily []struct {
		Metric string `db:"metric"`
		Value  int64  `db:"value"`
	}
	require.NoError(t, d.Owner.Select(&daily, "SELECT metric, value FROM usage_daily WHERE day = $1::date ORDER BY metric", old))
	require.Len(t, daily, 2)
	assert.Equal(t, "devices_peak", daily[0].Metric)
	assert.EqualValues(t, 7, daily[0].Value, "gauges roll up to their highest value")
	assert.EqualValues(t, 15, daily[1].Value, "counters add up")

	got := hourly(t, d)
	assert.EqualValues(t, 2, got["a/monitoring_rows"])
	assert.Zero(t, got["a/jobs_command"], "hours past the retention are pruned")
	var n int
	require.NoError(t, d.Owner.Get(&n, "SELECT count(*) FROM usage_device_days"))
	assert.Zero(t, n, "device-days past 25 months are pruned")

	// day reports read the rollup for old days
	s := Store{DB: d.App}
	q, err := metering.ParseQuery(old.Format("2006-01-02"), today().Format("2006-01-02"), "day", time.Now())
	require.NoError(t, err)
	data, err := s.Usage(ctx, repotest.TenantA, q)
	require.NoError(t, err)
	r := metering.BuildReport(q, data, time.Now())
	assert.EqualValues(t, 15, r.Totals.TunnelBytesToUser)
	assert.EqualValues(t, 7, r.Totals.DevicesPeak)
	// changing an old hour is reflected after the next upkeep (recomputed while late data may come)
	_, err = d.Owner.Exec("UPDATE usage_hourly SET value = 20 WHERE hour = $1 AND metric = 'tunnel_bytes_to_user'", old)
	require.NoError(t, err)
	require.NoError(t, Upkeep(ctx, d.App, Retention{}))
	data, err = s.Usage(ctx, repotest.TenantA, q)
	require.NoError(t, err)
	assert.EqualValues(t, 25, metering.BuildReport(q, data, time.Now()).Totals.TunnelBytesToUser)
}

// Days that passed the rollup cutoff are read from their hours until the upkeep rolled them up (no gap at midnight,
// nor while no node does the upkeep); without Monitoring the upkeep records no monitoring gauge.
func TestDaysNotRolledUpYetAreReadFromHours(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	old := today().AddDate(0, 0, -4).Add(5 * time.Hour)
	_, err := d.Owner.Exec(`INSERT INTO usage_hourly (tenant_id, hour, metric, value) VALUES ('a', $1, 'api_bytes_out', 11)`, old)
	require.NoError(t, err)
	_, err = d.Owner.Exec(`INSERT INTO measurements (tenant_id, client_id, timestamp, cpu_usage_percent, memory_usage_percent, io_usage_percent)
		VALUES ('a', 'c1', now(), 1, 1, 1)`)
	require.NoError(t, err)
	s := Store{DB: d.App}
	q, err := metering.ParseQuery(old.Format("2006-01-02"), "", "day", time.Now())
	require.NoError(t, err)
	read := func() int64 {
		data, err := s.Usage(ctx, repotest.TenantA, q)
		require.NoError(t, err)
		return metering.BuildReport(q, data, time.Now()).Totals.APIBytesOut
	}
	assert.EqualValues(t, 11, read(), "before the upkeep")
	require.NoError(t, Upkeep(ctx, d.App, Retention{}))
	assert.EqualValues(t, 11, read(), "after the upkeep, once")
	assert.Zero(t, hourly(t, d)["a/monitoring_rows"])
	rows, _, err := s.Summary(ctx, q.From, q.To)
	require.NoError(t, err)
	assert.Equal(t, []metering.TenantRow{{Tenant: "a", Metric: "api_bytes_out", Value: 11}}, rows)
}

// ingest_usage adds the transfer metrics of a record with a node id to usage_node_daily once (a replayed record
// adds nothing), leaves other metrics, records without a node and invalid node ids out; the upkeep prunes it.
func TestIngestCountsTransferPerNode(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	w := Writer{DB: d.App}
	h := thisHour()
	r1 := rec(repotest.TenantA, "k1",
		metering.Delta{Hour: h, Metric: metering.TunnelBytesToUser, Value: 1000},
		metering.Delta{Hour: h, Metric: metering.TunnelBytesToDevice, Value: 100},
		metering.Delta{Hour: h, Metric: metering.JobsCommand, Value: 2})
	r2 := rec(repotest.TenantB, "k1", metering.Delta{Hour: h, Metric: metering.TunnelBytesToUser, Value: 1})
	noNode := metering.Record{Tenant: repotest.TenantA, Key: "k2", Deltas: []metering.Delta{{Hour: h, Metric: metering.APIBytesOut, Value: 50}}}
	badNode := metering.Record{Tenant: repotest.TenantA, Node: "../x", Key: "k3", Deltas: []metering.Delta{{Hour: h, Metric: metering.APIBytesOut, Value: 50}}}
	require.NoError(t, w.WriteUsage(ctx, []metering.Record{r1, r2, noNode, badNode}))
	require.NoError(t, w.WriteUsage(ctx, []metering.Record{r1}), "replayed")

	var rows []struct {
		Node   string `db:"node_id"`
		Metric string `db:"metric"`
		Value  int64  `db:"value"`
	}
	require.NoError(t, d.Owner.Select(&rows, "SELECT node_id, metric, value FROM usage_node_daily WHERE day = $1::date ORDER BY metric", today()))
	require.Len(t, rows, 2, "%v", rows)
	assert.Equal(t, "n1", rows[0].Node)
	assert.Equal(t, "tunnel_bytes_to_device", rows[0].Metric)
	assert.EqualValues(t, 100, rows[0].Value)
	assert.Equal(t, "tunnel_bytes_to_user", rows[1].Metric)
	assert.EqualValues(t, 1001, rows[1].Value, "every tenant together")
	assert.EqualValues(t, 100, hourly(t, d)["a/api_bytes_out"], "the records without a valid node still count for the tenant")

	_, err := d.Owner.Exec("INSERT INTO usage_node_daily (node_id, day, metric, value) VALUES ('n1', $1, 'api_bytes_out', 1)", today().AddDate(-3, 0, 0))
	require.NoError(t, err)
	require.NoError(t, Upkeep(ctx, d.App, Retention{}))
	var n int
	require.NoError(t, d.Owner.Get(&n, "SELECT count(*) FROM usage_node_daily"))
	assert.Equal(t, 2, n, "days past 25 months are pruned")
}
