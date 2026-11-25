// Package metering counts what each organization (tenant) uses of a server on Postgres (billing and metering
// design, phase 1): tunnel bytes in both directions, upload bytes delivered to devices, API bytes, jobs run and the
// devices connected. A Meter keeps the counts in memory per (tenant, client, UTC hour, metric) and the client ids
// connected per UTC day, and flushes them every minute to a Writer, one Record per tenant: on a device process into
// the outbox (kind "usage"), on other nodes straight through the database's ingest_usage function, which adds each
// record once (its key is claimed in ingest_keys) into usage_hourly, usage_client_daily and usage_device_days.
//
// Metering is off on standalone servers (SQLite) and wherever no Meter is made: every method of a nil *Meter and of
// a nil *TunnelBytes does nothing, so the counting points cost a nil check.
package metering

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/tenant"
)

// Metric names a counter of usage_hourly.
type Metric string

// The metrics. Counters are added up; gauges (IsGauge) keep the highest value of their hour or day.
const (
	// TunnelBytesToUser: bytes a tunnel carried from the device toward the user (RDP screens, files pulled from a
	// device, HTTP responses: the server's egress toward users).
	TunnelBytesToUser Metric = "tunnel_bytes_to_user"
	// TunnelBytesToDevice: bytes a tunnel carried from the user toward the device.
	TunnelBytesToDevice Metric = "tunnel_bytes_to_device"
	// UploadBytesToDevices: bytes of uploaded files the devices read from the server (each device's copy counts).
	UploadBytesToDevices Metric = "upload_bytes_to_devices"
	// APIBytesOut: bytes of API and UI responses (websockets included).
	APIBytesOut Metric = "api_bytes_out"
	// JobsCommand, JobsScript, JobsLibrary: jobs started on a device, by kind (a multi-client job counts once per
	// device); their sum is jobs_run in reports.
	JobsCommand Metric = "jobs_command"
	JobsScript  Metric = "jobs_script"
	JobsLibrary Metric = "jobs_library"
	// DeviceSeconds: connected devices integrated over time; an hour's value divided by 3600 is the average number
	// of devices connected in the hour.
	DeviceSeconds Metric = "device_seconds"
	// DevicesPeak (gauge): the most devices connected at once in the hour, as sampled by one node.
	DevicesPeak Metric = "devices_peak"
	// MonitoringRows (gauge): monitoring measurements stored, counted daily by the cluster upkeep.
	MonitoringRows Metric = "monitoring_rows"
)

// Metrics are all metrics, in reporting order.
var Metrics = []Metric{TunnelBytesToUser, TunnelBytesToDevice, UploadBytesToDevices, APIBytesOut, JobsCommand,
	JobsScript, JobsLibrary, DeviceSeconds, DevicesPeak, MonitoringRows}

// IsGauge reports whether m keeps the highest value of its hour instead of a sum.
func IsGauge(m Metric) bool { return m == DevicesPeak || m == MonitoringRows }

// IsTransfer reports whether m counts toward "transfer" (tunnel bytes both ways and upload bytes delivered).
func IsTransfer(m Metric) bool {
	return m == TunnelBytesToUser || m == TunnelBytesToDevice || m == UploadBytesToDevices
}

// JobMetric is the metric a job counts under: a library entry, a script or a command.
func JobMetric(job *models.Job) Metric {
	switch {
	case job.LibraryEntryID != "":
		return JobsLibrary
	case job.IsScript:
		return JobsScript
	default:
		return JobsCommand
	}
}

// Delta is an amount of a metric in an hour (UTC), of one client when Client is set (transfer metrics).
type Delta struct {
	Hour   time.Time
	Metric Metric
	Client string
	Value  int64
}

// DeviceDay lists clients connected on Day (UTC midnight).
type DeviceDay struct {
	Day     time.Time
	Clients []string
}

// Record is one flush of one tenant from one node. Key is its idempotency key (node, process start, tenant, flush
// sequence): shipping it twice adds it once.
type Record struct {
	Tenant  tenant.ID
	Node    string
	Key     string
	Deltas  []Delta
	Devices []DeviceDay
}

// Writer stores flushed records. An error means none of them may have been stored; the Meter retries the same
// records (same keys) at the next flush, so a Writer must be idempotent by Key.
type Writer interface {
	WriteUsage(ctx context.Context, recs []Record) error
}

// ConnectedDevices returns the ids of the clients connected to this node now, per tenant.
type ConnectedDevices func(ctx context.Context) map[tenant.ID][]string

// Options configure a Meter.
type Options struct {
	// Node is the node id (in records and keys).
	Node string
	// Writer stores the records; required.
	Writer Writer
	// Devices lists the connected devices; nil on nodes without a client listener (no device metrics).
	Devices ConnectedDevices
	// SampleInterval is how often devices are sampled and tunnel counters read (default 15 s, at most FlushInterval).
	SampleInterval time.Duration
	// FlushInterval is how often counts are flushed to the Writer (default 1 min).
	FlushInterval time.Duration
	Log           *logger.Logger
	// Now replaces time.Now (tests).
	Now func() time.Time
}

type bucket struct {
	tid    tenant.ID
	client string
	hour   int64 // unix seconds of the hour's start
	metric Metric
}

type dayKey struct {
	tid tenant.ID
	day int64 // unix seconds of the day's start
}

// Meter collects the counts of a node. It is safe for concurrent use; a nil *Meter does nothing.
type Meter struct {
	opts Options
	boot string

	mu      sync.Mutex
	counts  map[bucket]int64
	peaks   map[bucket]int64           // the highest value seen this hour
	shipped map[bucket]int64           // the peak last flushed
	seen    map[dayKey]map[string]bool // client ids connected that day; true once flushed
	tunnels map[*TunnelBytes]struct{}
	last    time.Time // last device sample

	flushMu sync.Mutex // one flush at a time
	pending []Record   // flushed but not stored yet: retried with the same keys
	seq     int64
}

// New returns a Meter; call Run to sample and flush periodically, and Close to flush what is left.
func New(o Options) *Meter {
	if o.FlushInterval <= 0 {
		o.FlushInterval = time.Minute
	}
	if o.SampleInterval <= 0 {
		o.SampleInterval = min(15*time.Second, o.FlushInterval)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Meter{
		opts:    o,
		boot:    fmt.Sprintf("%d%s", time.Now().Unix(), randomHex(3)),
		counts:  map[bucket]int64{},
		peaks:   map[bucket]int64{},
		shipped: map[bucket]int64{},
		seen:    map[dayKey]map[string]bool{},
		tunnels: map[*TunnelBytes]struct{}{},
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hourOf(t time.Time) int64 { return t.UTC().Truncate(time.Hour).Unix() }

func dayOf(t time.Time) int64 {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).Unix()
}

func orDefault(tid tenant.ID) tenant.ID {
	if tid == "" {
		return tenant.Default
	}
	return tid
}

// Add counts n of metric for tid (and client, when not empty) in the current hour.
func (m *Meter) Add(tid tenant.ID, client string, metric Metric, n int64) {
	if m == nil || n <= 0 {
		return
	}
	k := bucket{tid: orDefault(tid), client: client, hour: hourOf(m.opts.Now()), metric: metric}
	m.mu.Lock()
	m.counts[k] += n
	m.mu.Unlock()
}

// AddCtx counts n of metric for the tenant of ctx (the default tenant without one).
func (m *Meter) AddCtx(ctx context.Context, client string, metric Metric, n int64) {
	if m == nil {
		return
	}
	tid, _ := tenant.FromContext(ctx)
	m.Add(tid, client, metric, n)
}

// Job counts a job started on a device of the tenant of ctx.
func (m *Meter) Job(ctx context.Context, job *models.Job) {
	if m == nil || job == nil {
		return
	}
	m.AddCtx(ctx, "", JobMetric(job), 1)
}

// Tunnel returns the byte counters of a new tunnel of device client of tid, read by the Meter until the tunnel is
// closed (TunnelBytes.Close). nil when m is nil.
func (m *Meter) Tunnel(tid tenant.ID, client string) *TunnelBytes {
	if m == nil {
		return nil
	}
	b := &TunnelBytes{tid: orDefault(tid), client: client}
	m.mu.Lock()
	m.tunnels[b] = struct{}{}
	m.mu.Unlock()
	return b
}

// Sample reads the tunnel counters and, when the node lists devices, samples the connected devices. Run calls it
// every SampleInterval.
func (m *Meter) Sample(ctx context.Context) {
	if m == nil {
		return
	}
	now := m.opts.Now()
	var devices map[tenant.ID][]string
	if m.opts.Devices != nil {
		devices = m.opts.Devices(ctx)
	}
	h := hourOf(now)
	m.mu.Lock()
	defer m.mu.Unlock()
	for b := range m.tunnels {
		closed := b.closed.Load() // read before the counters: bytes counted before Close are all seen below
		up, down := b.toDevice.Load(), b.toUser.Load()
		if d := down - b.seenToUser; d > 0 {
			m.counts[bucket{b.tid, b.client, h, TunnelBytesToUser}] += d
		}
		if d := up - b.seenToDevice; d > 0 {
			m.counts[bucket{b.tid, b.client, h, TunnelBytesToDevice}] += d
		}
		b.seenToUser, b.seenToDevice = down, up
		if closed {
			// a connection may still have been copying when the tunnel closed: keep it for one more sample
			if b.drained {
				delete(m.tunnels, b)
			}
			b.drained = true
		}
	}
	if m.opts.Devices == nil {
		return
	}
	var elapsed int64
	if !m.last.IsZero() && now.After(m.last) {
		elapsed = int64(now.Sub(m.last).Round(time.Second) / time.Second)
	}
	m.last = now
	day := dayOf(now)
	for tid, ids := range devices {
		if len(ids) == 0 {
			continue
		}
		tid = orDefault(tid)
		n := int64(len(ids))
		if elapsed > 0 {
			m.counts[bucket{tid, "", h, DeviceSeconds}] += n * elapsed
		}
		k := bucket{tid, "", h, DevicesPeak}
		if n > m.peaks[k] {
			m.peaks[k] = n
		}
		dk := dayKey{tid, day}
		set := m.seen[dk]
		if set == nil {
			set = map[string]bool{}
			m.seen[dk] = set
		}
		for _, id := range ids {
			if _, ok := set[id]; !ok {
				set[id] = false
			}
		}
	}
}

// Flush hands what was counted since the last flush to the Writer. Records a failed write held are written first,
// with their keys unchanged.
func (m *Meter) Flush(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.flushMu.Lock()
	defer m.flushMu.Unlock()
	if len(m.pending) > 0 {
		if err := m.opts.Writer.WriteUsage(ctx, m.pending); err != nil {
			return err
		}
		m.pending = nil
	}
	recs := m.take()
	if len(recs) == 0 {
		return nil
	}
	if err := m.opts.Writer.WriteUsage(ctx, recs); err != nil {
		m.pending = recs
		return err
	}
	return nil
}

// take moves the counts, the peaks that rose and the devices not flushed yet into one record per tenant.
func (m *Meter) take() []Record {
	m.seq++
	seq := m.seq
	now := m.opts.Now()
	curHour, curDay := hourOf(now), dayOf(now)
	m.mu.Lock()
	defer m.mu.Unlock()
	by := map[tenant.ID]*Record{}
	rec := func(tid tenant.ID) *Record {
		r := by[tid]
		if r == nil {
			r = &Record{Tenant: tid, Node: m.opts.Node, Key: m.key(tid, seq)}
			by[tid] = r
		}
		return r
	}
	for k, v := range m.counts {
		r := rec(k.tid)
		r.Deltas = append(r.Deltas, Delta{Hour: time.Unix(k.hour, 0).UTC(), Metric: k.metric, Client: k.client, Value: v})
	}
	clear(m.counts)
	for k, v := range m.peaks {
		if v > m.shipped[k] {
			r := rec(k.tid)
			r.Deltas = append(r.Deltas, Delta{Hour: time.Unix(k.hour, 0).UTC(), Metric: k.metric, Value: v})
			m.shipped[k] = v
		}
		if k.hour < curHour { // that hour is over: its peak is final
			delete(m.peaks, k)
			delete(m.shipped, k)
		}
	}
	for dk, set := range m.seen {
		var ids []string
		for id, done := range set {
			if !done {
				ids = append(ids, id)
				set[id] = true
			}
		}
		if len(ids) > 0 {
			sort.Strings(ids)
			r := rec(dk.tid)
			r.Devices = append(r.Devices, DeviceDay{Day: time.Unix(dk.day, 0).UTC(), Clients: ids})
		}
		if dk.day < curDay {
			delete(m.seen, dk)
		}
	}
	out := make([]Record, 0, len(by))
	for _, r := range by {
		sort.Slice(r.Deltas, func(i, j int) bool {
			a, b := r.Deltas[i], r.Deltas[j]
			if !a.Hour.Equal(b.Hour) {
				return a.Hour.Before(b.Hour)
			}
			if a.Metric != b.Metric {
				return a.Metric < b.Metric
			}
			return a.Client < b.Client
		})
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tenant < out[j].Tenant })
	return out
}

func (m *Meter) key(tid tenant.ID, seq int64) string {
	key := fmt.Sprintf("%s/%s/%s/%d", m.opts.Node, m.boot, tid, seq)
	if len(key) > 200 {
		sum := sha256.Sum256([]byte(key))
		key = hex.EncodeToString(sum[:])
	}
	return key
}

// Run samples every SampleInterval and flushes every FlushInterval until ctx is done. Call Close afterwards to
// flush the rest.
func (m *Meter) Run(ctx context.Context) {
	if m == nil {
		return
	}
	tick := time.NewTicker(m.opts.SampleInterval)
	defer tick.Stop()
	m.Sample(ctx)
	lastFlush := m.opts.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		m.Sample(ctx)
		if m.opts.Now().Sub(lastFlush) < m.opts.FlushInterval {
			continue
		}
		lastFlush = m.opts.Now()
		if err := m.Flush(ctx); err != nil && m.opts.Log != nil {
			m.opts.Log.Errorf("metering: flush failed, retrying at the next flush: %v", err)
		}
	}
}

// Close samples once more and flushes, waiting at most 10 s for the Writer. A failed final flush is logged, not
// returned: it is no reason to fail a shutdown.
func (m *Meter) Close() error {
	if m == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m.Sample(ctx)
	if err := m.Flush(ctx); err != nil && m.opts.Log != nil {
		m.opts.Log.Errorf("metering: final flush failed, the usage since the last flush is lost: %v", err)
	}
	return nil
}

// TunnelBytes counts the bytes of one tunnel. Counting is an atomic add per buffer copied; the Meter reads the
// totals every sample. A nil *TunnelBytes counts nothing.
type TunnelBytes struct {
	toUser   atomic.Int64
	toDevice atomic.Int64
	closed   atomic.Bool
	tid      tenant.ID
	client   string

	// guarded by Meter.mu
	seenToUser, seenToDevice int64
	drained                  bool
}

// ToUser counts n bytes sent from the device toward the user.
func (b *TunnelBytes) ToUser(n int64) {
	if b != nil && n > 0 {
		b.toUser.Add(n)
	}
}

// ToDevice counts n bytes sent from the user toward the device.
func (b *TunnelBytes) ToDevice(n int64) {
	if b != nil && n > 0 {
		b.toDevice.Add(n)
	}
}

// Totals are the bytes counted since the tunnel opened.
func (b *TunnelBytes) Totals() (toUser, toDevice int64) {
	if b == nil {
		return 0, 0
	}
	return b.toUser.Load(), b.toDevice.Load()
}

// Close ends the tunnel's counting: the Meter reads it a last time and forgets it. Safe to call more than once.
func (b *TunnelBytes) Close() {
	if b != nil {
		b.closed.Store(true)
	}
}

// MarshalJSON reports the totals (the per-tunnel bandwidth view).
func (b *TunnelBytes) MarshalJSON() ([]byte, error) {
	u, d := b.Totals()
	return []byte(fmt.Sprintf(`{"to_user":%d,"to_device":%d}`, u, d)), nil
}

type deltaJSON struct {
	Hour   string `json:"hour"`
	Metric Metric `json:"metric"`
	Value  int64  `json:"value"`
	Client string `json:"client_id,omitempty"`
}

type deviceDayJSON struct {
	Day     string   `json:"day"`
	Clients []string `json:"client_ids"`
}

type recordJSON struct {
	Tenant  tenant.ID       `json:"tenant_id"`
	Key     string          `json:"key"`
	Node    string          `json:"node_id,omitempty"`
	Deltas  []deltaJSON     `json:"deltas"`
	Devices []deviceDayJSON `json:"devices"`
}

// Payload is the record as ingest_usage takes it (one item of its batch). NULs are removed from ids: Postgres
// can't store them.
func (r Record) Payload() ([]byte, error) {
	j := recordJSON{Tenant: r.Tenant, Key: r.Key, Node: noNUL(r.Node), Deltas: []deltaJSON{}, Devices: []deviceDayJSON{}}
	for _, d := range r.Deltas {
		j.Deltas = append(j.Deltas, deltaJSON{Hour: d.Hour.UTC().Format(time.RFC3339), Metric: d.Metric, Value: d.Value, Client: noNUL(d.Client)})
	}
	for _, d := range r.Devices {
		ids := make([]string, len(d.Clients))
		for i, id := range d.Clients {
			ids[i] = noNUL(id)
		}
		j.Devices = append(j.Devices, deviceDayJSON{Day: d.Day.UTC().Format("2006-01-02"), Clients: ids})
	}
	return json.Marshal(j)
}

func noNUL(s string) string { return strings.ReplaceAll(s, "\x00", "") }

// UnmarshalJSON ignores the totals: a tunnel read back from storage is not counted.
func (b *TunnelBytes) UnmarshalJSON([]byte) error { return nil }
