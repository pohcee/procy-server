package outbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/share/tenant"
)

var ctx = context.Background()

func open(t *testing.T, path string, limit int) *Outbox {
	t.Helper()
	o, err := Open(path, Options{MaxPerTenant: limit})
	require.NoError(t, err)
	t.Cleanup(func() { o.Close() })
	return o
}

func keys(t *testing.T, o *Outbox, kind Kind) []string {
	t.Helper()
	items, err := o.Pending(ctx, kind, 100)
	require.NoError(t, err)
	out := []string{}
	for _, it := range items {
		out = append(out, it.Key)
	}
	return out
}

func TestSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.db")
	o, err := Open(path, Options{})
	require.NoError(t, err)
	require.NoError(t, o.Add(ctx, KindJobResult, "a", "j1", []byte(`{"x":1}`)))
	require.NoError(t, o.Add(ctx, KindJobResult, "a", "j1", []byte(`{"x":2}`)), "same key: ignored")
	require.NoError(t, o.Close())

	o = open(t, path, 0)
	items, err := o.Pending(ctx, KindJobResult, 10)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, `{"x":1}`, string(items[0].Payload))
	assert.Equal(t, tenant.ID("a"), items[0].Tenant)
}

func TestCapDropsMeasurementsFirstNeverJobResults(t *testing.T) {
	o := open(t, filepath.Join(t.TempDir(), "outbox.db"), 4)
	add := func(kind Kind, tid tenant.ID, key string) {
		t.Helper()
		require.NoError(t, o.Add(ctx, kind, tid, key, []byte(`{}`)))
	}
	add(KindAudit, "a", "au1")
	add(KindMeasurement, "a", "m1")
	add(KindMeasurement, "a", "m2")
	add(KindJobResult, "a", "j1")
	add(KindMeasurement, "b", "other-tenant") // b has its own cap

	add(KindJobResult, "a", "j2") // full: drops m1
	assert.Equal(t, []string{"m2", "other-tenant"}, keys(t, o, KindMeasurement))
	add(KindJobResult, "a", "j3") // drops m2
	add(KindJobResult, "a", "j4") // no measurement left: drops the audit entry
	assert.Equal(t, []string{"other-tenant"}, keys(t, o, KindMeasurement))
	assert.Empty(t, keys(t, o, KindAudit))

	add(KindJobResult, "a", "j5")   // only job results: stored beyond the cap
	add(KindMeasurement, "a", "m3") // ... and anything else is dropped
	assert.Equal(t, []string{"j1", "j2", "j3", "j4", "j5"}, keys(t, o, KindJobResult))
	assert.Equal(t, []string{"other-tenant"}, keys(t, o, KindMeasurement))
	assert.EqualValues(t, 4, o.Dropped())
	n, err := o.Count(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, 5, n)
}

type fakeSink struct {
	mu      sync.Mutex
	down    bool
	shipped map[Kind][]string
	reject  map[string]bool
	skip    map[string]bool
	calls   int
}

func (s *fakeSink) Ship(_ context.Context, kind Kind, items []Item) ([]Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.down {
		return nil, errors.New("connection refused")
	}
	var out []Status
	for _, it := range items {
		switch {
		case s.skip[it.Key]:
			continue
		case s.reject[it.Key]:
			out = append(out, Status{Key: it.Key, Outcome: "rejected: bad"})
		default:
			out = append(out, Status{Key: it.Key, Outcome: "applied"})
			if s.shipped == nil {
				s.shipped = map[Kind][]string{}
			}
			s.shipped[kind] = append(s.shipped[kind], it.Key)
		}
	}
	return out, nil
}

func TestShipAllBatchesAndRemoves(t *testing.T) {
	o := open(t, filepath.Join(t.TempDir(), "outbox.db"), 0)
	for i := 0; i < 7; i++ {
		require.NoError(t, o.Add(ctx, KindMeasurement, "a", fmt.Sprintf("m%d", i), []byte(`{}`)))
	}
	require.NoError(t, o.Add(ctx, KindJobResult, "a", "j", []byte(`{}`)))
	require.NoError(t, o.Add(ctx, KindAudit, "a", "bad", []byte(`{}`)))
	require.NoError(t, o.Add(ctx, KindAudit, "a", "unanswered", []byte(`{}`)))
	sink := &fakeSink{reject: map[string]bool{"bad": true}, skip: map[string]bool{"unanswered": true}}
	s := &Shipper{Outbox: o, Sink: sink, Batch: 3}

	n, err := s.ShipAll(ctx)
	require.NoError(t, err)
	assert.Equal(t, 9, n, "applied and rejected items are removed")
	assert.Equal(t, []string{"j"}, sink.shipped[KindJobResult])
	assert.Equal(t, []string{"m0", "m1", "m2", "m3", "m4", "m5", "m6"}, sink.shipped[KindMeasurement])
	assert.Equal(t, []string{"unanswered"}, keys(t, o, KindAudit), "an item the sink didn't answer for stays")
}

// While the database is down nothing is lost; once it is back the shipper delivers everything, and items added
// before a restart are shipped after it.
func TestShipperRetriesAfterOutage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.db")
	o, err := Open(path, Options{})
	require.NoError(t, err)
	require.NoError(t, o.Add(ctx, KindJobResult, "a", "before-restart", []byte(`{}`)))
	require.NoError(t, o.Close())

	o = open(t, path, 0)
	sink := &fakeSink{down: true}
	s := &Shipper{Outbox: o, Sink: sink, Interval: 20 * time.Millisecond, MinBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { s.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	require.NoError(t, o.Add(ctx, KindJobResult, "a", "during-outage", []byte(`{}`)))
	require.Eventually(t, func() bool { sink.mu.Lock(); defer sink.mu.Unlock(); return sink.calls >= 3 }, 2*time.Second, 5*time.Millisecond)
	n, err := o.Count(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, 2, n, "kept while the database is down")

	sink.mu.Lock()
	sink.down = false
	sink.mu.Unlock()
	require.Eventually(t, func() bool { n, _ := o.Count(ctx, ""); return n == 0 }, 2*time.Second, 5*time.Millisecond)
	sink.mu.Lock()
	assert.Equal(t, []string{"before-restart", "during-outage"}, sink.shipped[KindJobResult])
	sink.mu.Unlock()
}

type rejectingSink struct{ failKind Kind }

func (s rejectingSink) Ship(_ context.Context, kind Kind, items []Item) ([]Status, error) {
	if kind == s.failKind {
		return nil, errors.New("down for this kind")
	}
	out := make([]Status, len(items))
	for i, it := range items {
		out[i] = Status{Key: it.Key, Outcome: "rejected: no"}
	}
	return out, nil
}

// A rejected job result is kept in the dead letters, never dropped; another kind's failure doesn't stop it.
func TestRejectedJobResultsAreKept(t *testing.T) {
	o := open(t, filepath.Join(t.TempDir(), "outbox.db"), 0)
	require.NoError(t, o.Add(ctx, KindJobResult, "a", "j", []byte(`{}`)))
	require.NoError(t, o.Add(ctx, KindMeasurement, "a", "m", []byte(`{}`)))
	require.NoError(t, o.Add(ctx, KindAudit, "a", "au", []byte(`{}`)))
	s := &Shipper{Outbox: o, Sink: rejectingSink{failKind: KindClientState}}
	require.NoError(t, o.Add(ctx, KindClientState, "a", "st", []byte(`{}`)))
	n, err := s.ShipAll(ctx)
	require.Error(t, err, "client state failed")
	assert.Equal(t, 3, n, "the other kinds were shipped")
	dl, err := o.DeadLetters(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, dl, "the job result is kept")
	assert.Equal(t, []string{"st"}, keys(t, o, KindClientState))
	assert.Empty(t, keys(t, o, KindAudit), "a rejected audit entry is dropped")
}

type countingSink struct{ sizes []int }

func (s *countingSink) Ship(_ context.Context, _ Kind, items []Item) ([]Status, error) {
	s.sizes = append(s.sizes, len(items))
	out := make([]Status, len(items))
	for i, it := range items {
		out[i] = Status{Key: it.Key, Outcome: "applied"}
	}
	return out, nil
}

func TestBatchesCappedByBytes(t *testing.T) {
	o := open(t, filepath.Join(t.TempDir(), "outbox.db"), 0)
	big := make([]byte, 400)
	for i := range big {
		big[i] = 'x'
	}
	for i := 0; i < 5; i++ {
		require.NoError(t, o.Add(ctx, KindJobResult, "a", fmt.Sprintf("j%d", i), big))
	}
	sink := &countingSink{}
	n, err := (&Shipper{Outbox: o, Sink: sink, MaxBatchBytes: 1000}).ShipAll(ctx)
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, []int{2, 2, 1}, sink.sizes)

	// an item larger than the cap still ships, alone
	require.NoError(t, o.Add(ctx, KindJobResult, "a", "huge", make([]byte, 5000)))
	sink.sizes = nil
	_, err = (&Shipper{Outbox: o, Sink: sink, MaxBatchBytes: 1000}).ShipAll(ctx)
	require.NoError(t, err)
	assert.Equal(t, []int{1}, sink.sizes)
}

// Keys repeat across tenants: statuses in order are matched by position.
func TestSameKeyInTwoTenants(t *testing.T) {
	o := open(t, filepath.Join(t.TempDir(), "outbox.db"), 0)
	require.NoError(t, o.Add(ctx, KindMeasurement, "a", "k", []byte(`{}`)))
	require.NoError(t, o.Add(ctx, KindMeasurement, "b", "k", []byte(`{}`)))
	n, err := (&Shipper{Outbox: o, Sink: &countingSink{}}).ShipAll(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}
