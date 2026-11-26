package device

import (
	"context"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/auditlog"
	"github.com/pohcee/procy-server/server/clientsauth"
	"github.com/pohcee/procy-server/server/repotest"
	"github.com/pohcee/procy-server/server/storage/outbox"
	"github.com/pohcee/procy-server/server/storage/postgres"
	pgjobs "github.com/pohcee/procy-server/server/storage/postgres/jobs"
	"github.com/pohcee/procy-server/server/storage/postgres/pgtest"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/models"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var testLog = logger.NewLogger("test", logger.LogOutput{}, logger.LogLevelError)

func openOutbox(t *testing.T, path string) *outbox.Outbox {
	t.Helper()
	o, err := outbox.Open(path, outbox.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { o.Close() })
	return o
}

func TestClientAuthVerifiesHashes(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	h, err := clientsauth.HashPassword("pw-a")
	require.NoError(t, err)
	_, err = d.Owner.Exec("INSERT INTO clients_auth (tenant_id, id, password) VALUES ('a', 'auth-a', $1), ('a', 'plain', 'pw')", h)
	require.NoError(t, err)

	ca := ClientAuth{DB: d.Device}
	a, b := repotest.Ctx(repotest.TenantA), repotest.Ctx(repotest.TenantB)
	for _, c := range []struct {
		name   string
		ctx    context.Context
		id, pw string
		want   bool
	}{
		{"right password", a, "auth-a", "pw-a", true},
		{"wrong password", a, "auth-a", "pw-b", false},
		{"another tenant's credential", b, "auth-a", "pw-a", false},
		{"unknown id", a, "nope", "pw-a", false},
		{"stored unhashed: never seen by the device role", a, "plain", "pw", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ok, err := clientsauth.Verify(c.ctx, ca, c.id, []byte(c.pw))
			require.NoError(t, err)
			assert.Equal(t, c.want, ok)
		})
	}
	tid, _, found, err := ca.Lookup(context.Background(), "auth-a")
	require.NoError(t, err)
	assert.True(t, found)
	assert.EqualValues(t, "a", tid, "the lookup finds the tenant of an id")
	got, err := ca.Get(a, "auth-a")
	require.NoError(t, err)
	assert.Equal(t, &clientsauth.ClientAuth{ID: "auth-a"}, got)
	assert.False(t, ca.IsWriteable())
}

func TestDeviceStoresShipThroughIngest(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	_, err := d.Owner.Exec("INSERT INTO clients_auth (tenant_id, id, password) VALUES ('a', 'auth-c1', 'x')")
	require.NoError(t, err)
	ob := openOutbox(t, filepath.Join(t.TempDir(), "outbox.db"))
	ctx := repotest.Ctx(repotest.TenantA)

	cs := &Clients{DB: d.Device, Outbox: ob, NodeID: "n1"}
	c := repotest.StoreClient("c1", nil)
	c.SetAllowedUserGroups([]string{"sneaky"})
	require.NoError(t, cs.Save(ctx, c))
	require.NoError(t, cs.Save(ctx, c), "no second connection event while still connected")
	at := time.Now().UTC().Truncate(time.Second)
	c.SetDisconnectedAt(&at)
	require.NoError(t, cs.Save(ctx, c))

	fin := time.Date(2026, 11, 24, 10, 0, 1, 0, time.UTC)
	job := &models.Job{JID: "j1", Status: models.JobStatusSuccessful, FinishedAt: &fin,
		ClientID: "c1", ClientName: "c1", Command: "echo hi", CreatedBy: "admin", StartedAt: fin.Add(-time.Second),
		Result: &models.JobResult{StdOut: "hi"}}
	require.NoError(t, Jobs{Outbox: ob}.SaveJob(ctx, job))
	require.NoError(t, Monitoring{Outbox: ob}.CreateMeasurement(ctx, &models.Measurement{ClientID: "c1", Timestamp: fin, CPUUsagePercent: 5, NetLan: &models.NetBytes{In: 1, Out: 2}}))
	require.NoError(t, Monitoring{Outbox: ob}.CreateMeasurement(ctx, &models.Measurement{ClientID: "c1", Timestamp: fin, CPUUsagePercent: 6}), "same time: kept once")
	require.NoError(t, AuditLog{Outbox: ob}.Save(ctx, &auditlog.Entry{Timestamp: fin, Application: "client.command", Action: "execute.done", ClientID: "c1"}))

	s := &outbox.Shipper{Outbox: ob, Sink: Sink{DB: d.Device}, Log: testLog}
	n, err := s.ShipAll(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 8, n, "3 states, 2 connection events, 1 job, 1 measurement (the second one at the same time is ignored), 1 audit entry")
	left, err := ob.Count(context.Background(), "")
	require.NoError(t, err)
	assert.Zero(t, left)

	// the api side sees it all through its own stores
	got, err := pgjobs.New(d.App).GetByJID(ctx, "c1", "j1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "hi", got.Result.StdOut)
	assert.Equal(t, models.JobStatusSuccessful, got.Status)

	var events []string
	require.NoError(t, postgresSelect(d, &events, "SELECT event FROM client_connection_events ORDER BY id"))
	assert.Equal(t, []string{"connected", "disconnected"}, events)
	var cpu float64
	require.NoError(t, d.Owner.Get(&cpu, "SELECT cpu_usage_percent FROM measurements"))
	assert.Equal(t, 5.0, cpu)
	var audits int
	require.NoError(t, d.Owner.Get(&audits, "SELECT count(*) FROM auditlog WHERE action = 'execute.done'"))
	assert.Equal(t, 1, audits)

	// the device process reads the client back as the API sees it: no ACL a device reported
	back, err := cs.Get(ctx, "c1", testLog)
	require.NoError(t, err)
	require.NotNil(t, back)
	assert.Empty(t, back.GetAllowedUserGroups())
	assert.NotNil(t, back.GetDisconnectedAt())
}

func postgresSelect(d *pgtest.DB, dest any, q string) error { return d.Owner.Select(dest, q) }

// proxy forwards TCP connections to target until cut; restore listens again on the same address.
type proxy struct {
	target, addr string
	mu           sync.Mutex
	l            net.Listener
	conns        []net.Conn
}

func newProxy(t *testing.T, target string) *proxy {
	t.Helper()
	p := &proxy{target: target}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p.addr = l.Addr().String()
	p.serve(l)
	t.Cleanup(p.cut)
	return p
}

func (p *proxy) serve(l net.Listener) {
	p.mu.Lock()
	p.l = l
	p.mu.Unlock()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", p.target)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, up)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
			go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
		}
	}()
}

func (p *proxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.l != nil {
		_ = p.l.Close()
		p.l = nil
	}
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

func (p *proxy) restore(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp", p.addr)
	require.NoError(t, err)
	p.serve(l)
}

// While the database is unreachable the device process keeps what devices report in the outbox, also across a
// restart; once the database is back the shipper delivers all of it.
func TestOutboxShipsAfterDatabaseOutage(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	u, err := url.Parse(d.DeviceURL)
	require.NoError(t, err)
	px := newProxy(t, u.Host)
	u.Host = px.addr
	db, err := postgres.Open(context.Background(), u.String())
	require.NoError(t, err)
	defer db.Close()

	path := filepath.Join(t.TempDir(), "outbox.db")
	ctx := repotest.Ctx(repotest.TenantA)
	ob, err := outbox.Open(path, outbox.Options{})
	require.NoError(t, err)
	px.cut()
	audit := AuditLog{Outbox: ob}
	require.NoError(t, audit.Save(ctx, &auditlog.Entry{Timestamp: time.Now(), Application: "a", Action: "during-outage"}))
	s := &outbox.Shipper{Outbox: ob, Sink: Sink{DB: db}}
	shipCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err = s.ShipAll(shipCtx)
	cancel()
	require.Error(t, err, "the database is unreachable")
	require.NoError(t, ob.Close())

	// restart: a new outbox on the same file, the database back
	ob = openOutbox(t, path)
	require.NoError(t, AuditLog{Outbox: ob}.Save(ctx, &auditlog.Entry{Timestamp: time.Now(), Application: "a", Action: "after-restart"}))
	px.restore(t)
	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	go (&outbox.Shipper{Outbox: ob, Sink: Sink{DB: db}, MinBackoff: 10 * time.Millisecond, Interval: 50 * time.Millisecond}).Run(runCtx)
	require.Eventually(t, func() bool {
		var n int
		_ = d.Owner.Get(&n, "SELECT count(*) FROM auditlog WHERE action IN ('during-outage', 'after-restart')")
		return n == 2
	}, 10*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool { n, _ := ob.Count(context.Background(), ""); return n == 0 }, 5*time.Second, 20*time.Millisecond)
}

// An item the database can't store (a raw NUL in a string fails the jsonb cast of the whole batch) is rejected on
// its own; the other items of the batch are applied.
func TestSinkIsolatesBadItem(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	ob := openOutbox(t, filepath.Join(t.TempDir(), "outbox.db"))
	ctx := context.Background()
	require.NoError(t, ob.Add(ctx, outbox.KindAudit, "a", "good1", []byte(`{"tenant_id":"a","key":"good1","timestamp":"2026-11-24T10:00:00Z","application":"x","action":"y"}`)))
	require.NoError(t, ob.Add(ctx, outbox.KindAudit, "a", "nul", []byte(`{"tenant_id":"a","key":"nul","timestamp":"2026-11-24T10:00:00Z","application":"x\u0000","action":"y"}`)))
	require.NoError(t, ob.Add(ctx, outbox.KindAudit, "a", "good2", []byte(`{"tenant_id":"a","key":"good2","timestamp":"2026-11-24T10:00:00Z","application":"x","action":"y"}`)))
	n, err := (&outbox.Shipper{Outbox: ob, Sink: Sink{DB: d.Device}, Log: testLog}).ShipAll(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	var audits int
	require.NoError(t, d.Owner.Get(&audits, "SELECT count(*) FROM auditlog"))
	assert.Equal(t, 2, audits)

	// the stores strip NULs before they reach the outbox
	require.NoError(t, AuditLog{Outbox: ob}.Save(repotest.Ctx(repotest.TenantA), &auditlog.Entry{Timestamp: time.Now(), Application: "a\x00b", Action: "z"}))
	_, err = (&outbox.Shipper{Outbox: ob, Sink: Sink{DB: d.Device}}).ShipAll(ctx)
	require.NoError(t, err)
	var app string
	require.NoError(t, d.Owner.Get(&app, "SELECT application FROM auditlog WHERE action = 'z'"))
	assert.Equal(t, "ab", app)
}
