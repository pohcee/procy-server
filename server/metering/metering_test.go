package metering

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/tenant"
)

type memWriter struct {
	mu   sync.Mutex
	recs []Record
	fail error
}

func (w *memWriter) WriteUsage(_ context.Context, recs []Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail != nil {
		return w.fail
	}
	w.recs = append(w.recs, recs...)
	return nil
}

// sums adds up the deltas written per (tenant, metric); gauges keep the highest.
func (w *memWriter) sums() map[tenant.ID]map[Metric]int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[tenant.ID]map[Metric]int64{}
	seen := map[string]bool{}
	for _, r := range w.recs {
		if seen[r.Key] { // the database adds a key once
			continue
		}
		seen[r.Key] = true
		if out[r.Tenant] == nil {
			out[r.Tenant] = map[Metric]int64{}
		}
		for _, d := range r.Deltas {
			if IsGauge(d.Metric) {
				out[r.Tenant][d.Metric] = max(out[r.Tenant][d.Metric], d.Value)
			} else {
				out[r.Tenant][d.Metric] += d.Value
			}
		}
	}
	return out
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestNilMeterAndTunnelBytesDoNothing(t *testing.T) {
	var m *Meter
	m.Add("a", "", JobsCommand, 1)
	m.Job(context.Background(), &models.Job{})
	m.Sample(context.Background())
	assert.NoError(t, m.Flush(context.Background()))
	assert.NoError(t, m.Close())
	b := m.Tunnel("a", "c")
	assert.Nil(t, b)
	b.ToUser(5)
	b.ToDevice(5)
	b.Close()
	u, d := b.Totals()
	assert.Zero(t, u+d)
}

func TestCountsFlushPerTenantAndHour(t *testing.T) {
	ck := &clock{t: time.Date(2026, 11, 24, 10, 59, 0, 0, time.UTC)}
	w := &memWriter{}
	m := New(Options{Node: "n1", Writer: w, Now: ck.now})
	ctx := tenant.WithID(context.Background(), "a")

	m.Job(ctx, &models.Job{Command: "ls"})
	m.Job(ctx, &models.Job{IsScript: true})
	m.Job(ctx, &models.Job{LibraryEntryID: "e", IsScript: true})
	m.Add("b", "c1", UploadBytesToDevices, 100)
	ck.add(2 * time.Minute) // the next hour
	m.Add("b", "c1", UploadBytesToDevices, 50)
	require.NoError(t, m.Flush(ctx))

	require.Len(t, w.recs, 2, "one record per tenant")
	byTenant := map[tenant.ID]Record{}
	for _, r := range w.recs {
		byTenant[r.Tenant] = r
		assert.Contains(t, r.Key, "n1/")
		assert.Contains(t, r.Key, "/"+string(r.Tenant)+"/")
	}
	assert.ElementsMatch(t, []Delta{
		{Hour: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC), Metric: JobsCommand, Value: 1},
		{Hour: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC), Metric: JobsScript, Value: 1},
		{Hour: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC), Metric: JobsLibrary, Value: 1},
	}, byTenant["a"].Deltas)
	assert.Equal(t, []Delta{
		{Hour: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC), Metric: UploadBytesToDevices, Client: "c1", Value: 100},
		{Hour: time.Date(2026, 11, 24, 11, 0, 0, 0, time.UTC), Metric: UploadBytesToDevices, Client: "c1", Value: 50},
	}, byTenant["b"].Deltas)

	// nothing new: nothing written
	require.NoError(t, m.Flush(ctx))
	assert.Len(t, w.recs, 2)
}

func TestFailedFlushIsRetriedWithTheSameKeys(t *testing.T) {
	w := &memWriter{fail: errors.New("database down")}
	m := New(Options{Node: "n1", Writer: w})
	m.Add("a", "", JobsCommand, 3)
	require.Error(t, m.Flush(context.Background()))
	m.Add("a", "", JobsCommand, 2)
	require.Error(t, m.Flush(context.Background()))
	w.fail = nil
	require.NoError(t, m.Flush(context.Background()))
	require.Len(t, w.recs, 2, "the held record first, then the new counts")
	assert.EqualValues(t, 3, w.recs[0].Deltas[0].Value)
	assert.EqualValues(t, 2, w.recs[1].Deltas[0].Value)
	assert.NotEqual(t, w.recs[0].Key, w.recs[1].Key)

	// a write that reached the database but failed afterwards is sent again with its key: added once
	w2 := &memWriter{}
	m2 := New(Options{Node: "n1", Writer: w2})
	m2.Add("a", "", JobsCommand, 4)
	require.NoError(t, m2.Flush(context.Background()))
	w2.recs = append(w2.recs, w2.recs...) // replayed
	assert.EqualValues(t, 4, w2.sums()["a"][JobsCommand])
}

func TestKeysDifferAcrossRestarts(t *testing.T) {
	w := &memWriter{}
	New(Options{Node: "n1", Writer: w}).Add("a", "", JobsCommand, 1)
	m1 := New(Options{Node: "n1", Writer: w})
	m2 := New(Options{Node: "n1", Writer: w})
	m1.Add("a", "", JobsCommand, 1)
	m2.Add("a", "", JobsCommand, 1)
	require.NoError(t, m1.Flush(context.Background()))
	require.NoError(t, m2.Flush(context.Background()))
	require.Len(t, w.recs, 2)
	assert.NotEqual(t, w.recs[0].Key, w.recs[1].Key, "two processes of one node never share a key")
}

func TestTunnelBytesAreReadWhileTheTunnelRuns(t *testing.T) {
	ck := &clock{t: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)}
	w := &memWriter{}
	m := New(Options{Node: "n1", Writer: w, Now: ck.now})
	b := m.Tunnel("a", "c1")
	b.ToUser(1000)
	b.ToDevice(10)
	m.Sample(context.Background())
	b.ToUser(500)
	m.Sample(context.Background())
	require.NoError(t, m.Flush(context.Background()))
	assert.EqualValues(t, 1500, w.sums()["a"][TunnelBytesToUser])
	assert.EqualValues(t, 10, w.sums()["a"][TunnelBytesToDevice])
	u, d := b.Totals()
	assert.EqualValues(t, 1500, u, "the per-tunnel totals keep counting")
	assert.EqualValues(t, 10, d)
	j, err := json.Marshal(struct {
		B *TunnelBytes `json:"bytes,omitempty"`
	}{b})
	require.NoError(t, err)
	assert.JSONEq(t, `{"bytes":{"to_user":1500,"to_device":10}}`, string(j))

	// bytes counted just after the close are still read once, then the tunnel is forgotten
	b.Close()
	b.ToUser(7)
	m.Sample(context.Background())
	m.Sample(context.Background())
	m.mu.Lock()
	assert.Empty(t, m.tunnels)
	m.mu.Unlock()
	require.NoError(t, m.Flush(context.Background()))
	assert.EqualValues(t, 1507, w.sums()["a"][TunnelBytesToUser])
}

func TestDevicesSampledPerHourAndDay(t *testing.T) {
	ck := &clock{t: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)}
	connected := map[tenant.ID][]string{"a": {"c1", "c2"}}
	var mu sync.Mutex
	w := &memWriter{}
	m := New(Options{Node: "n1", Writer: w, Now: ck.now, Devices: func(context.Context) map[tenant.ID][]string {
		mu.Lock()
		defer mu.Unlock()
		out := map[tenant.ID][]string{}
		for k, v := range connected {
			out[k] = append([]string(nil), v...)
		}
		return out
	}})
	ctx := context.Background()
	m.Sample(ctx) // first sample: the peak, no time yet
	ck.add(30 * time.Minute)
	mu.Lock()
	connected["a"] = []string{"c1", "c2", "c3", "c4"}
	mu.Unlock()
	m.Sample(ctx) // 4 devices for 30 min
	ck.add(30 * time.Minute)
	mu.Lock()
	connected["a"] = []string{"c1"}
	mu.Unlock()
	m.Sample(ctx) // 1 device for 30 min, now in the next hour
	require.NoError(t, m.Flush(ctx))

	var peak10, peak11, seconds10, seconds11 int64
	var days []DeviceDay
	for _, r := range w.recs {
		for _, d := range r.Deltas {
			switch {
			case d.Metric == DevicesPeak && d.Hour.Hour() == 10:
				peak10 = d.Value
			case d.Metric == DevicesPeak && d.Hour.Hour() == 11:
				peak11 = d.Value
			case d.Metric == DeviceSeconds && d.Hour.Hour() == 10:
				seconds10 += d.Value
			case d.Metric == DeviceSeconds && d.Hour.Hour() == 11:
				seconds11 += d.Value
			}
		}
		days = append(days, r.Devices...)
	}
	assert.EqualValues(t, 4, peak10)
	assert.EqualValues(t, 1, peak11)
	assert.EqualValues(t, 4*1800, seconds10)
	assert.EqualValues(t, 1*1800, seconds11)
	require.Len(t, days, 1)
	assert.Equal(t, []string{"c1", "c2", "c3", "c4"}, days[0].Clients)

	// a peak that didn't rise and devices already reported are not sent again
	m.Sample(ctx)
	w.recs = nil
	require.NoError(t, m.Flush(ctx))
	for _, r := range w.recs {
		assert.Empty(t, r.Devices)
		for _, d := range r.Deltas {
			assert.NotEqual(t, DevicesPeak, d.Metric)
		}
	}
}

func TestPayloadShape(t *testing.T) {
	r := Record{Tenant: "a", Node: "n\x001", Key: "k", Deltas: []Delta{{Hour: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC), Metric: TunnelBytesToUser, Client: "c\x001", Value: 5}},
		Devices: []DeviceDay{{Day: time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC), Clients: []string{"c1"}}}}
	p, err := r.Payload()
	require.NoError(t, err)
	assert.JSONEq(t, `{"tenant_id":"a","key":"k","node_id":"n1",
		"deltas":[{"hour":"2026-11-24T10:00:00Z","metric":"tunnel_bytes_to_user","value":5,"client_id":"c1"}],
		"devices":[{"day":"2026-11-24","client_ids":["c1"]}]}`, string(p))
}

func TestRunFlushesPeriodically(t *testing.T) {
	w := &memWriter{}
	m := New(Options{Node: "n1", Writer: w, SampleInterval: 5 * time.Millisecond, FlushInterval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	m.Add("a", "", JobsCommand, 1)
	require.Eventually(t, func() bool { return w.sums()["a"][JobsCommand] == 1 }, 2*time.Second, 5*time.Millisecond)
	cancel()
	<-done
	m.Add("a", "", JobsCommand, 1)
	require.NoError(t, m.Close())
	assert.EqualValues(t, 2, w.sums()["a"][JobsCommand], "Close flushes the rest")
}

// BenchmarkTunnelCounting is the cost of counting on the tunnel's copy path: one atomic add per buffer.
func BenchmarkTunnelCounting(b *testing.B) {
	m := New(Options{Node: "n1", Writer: &memWriter{}})
	tb := m.Tunnel("a", "c1")
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tb.ToUser(32 << 10)
		}
	})
}

// BenchmarkAdd is the cost of counting an event (a job, an upload read, an API response write).
func BenchmarkAdd(b *testing.B) {
	m := New(Options{Node: "n1", Writer: &memWriter{}})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m.Add("a", "c1", APIBytesOut, 512)
	}
}

// A big flush is split into records ingest_usage accepts, each with its own key; counts that could not be written
// for longer than ingest_usage accepts are dropped instead of making the whole flush rejected.
func TestLargeAndStaleFlushes(t *testing.T) {
	ck := &clock{t: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)}
	w := &memWriter{fail: errors.New("down")}
	m := New(Options{Node: "n1", Writer: w, Now: ck.now})
	m.Add("a", "old", APIBytesOut, 1)
	require.Error(t, m.Flush(context.Background())) // held as pending
	ck.add(7 * 24 * time.Hour)
	for i := 0; i < maxDeltas+10; i++ {
		m.Add("a", fmt.Sprintf("c%05d", i), UploadBytesToDevices, 1)
	}
	m.Add("a", "stale", APIBytesOut, 1)
	m.mu.Lock()
	m.counts[bucket{"a", "stale", hourOf(ck.now().Add(-maxAge - time.Hour)), APIBytesOut}] = 5
	m.mu.Unlock()
	w.fail = nil
	require.NoError(t, m.Flush(context.Background()))
	require.Len(t, w.recs, 3, "the pending record, then the new counts in two parts")
	keys := map[string]bool{}
	total := int64(0)
	for _, r := range w.recs[1:] {
		assert.LessOrEqual(t, len(r.Deltas), maxDeltas)
		keys[r.Key] = true
		for _, d := range r.Deltas {
			total += d.Value
		}
	}
	assert.Len(t, keys, 2)
	assert.EqualValues(t, maxDeltas+10+1, total, "the stale count is dropped")
}

func TestTunnelBytesStoredSnapshot(t *testing.T) {
	var v struct {
		B *TunnelBytes `json:"bytes,omitempty"`
	}
	require.NoError(t, json.Unmarshal([]byte(`{"bytes":{"to_user":12,"to_device":3}}`), &v))
	u, d := v.B.Totals()
	assert.EqualValues(t, 12, u)
	assert.EqualValues(t, 3, d)
	require.NoError(t, json.Unmarshal([]byte(`{"bytes":"garbage"}`), &v))
}
