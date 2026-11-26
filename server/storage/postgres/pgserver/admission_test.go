package pgserver

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	chserver "github.com/pohcee/procy-server/server"
	"github.com/pohcee/procy-server/server/storage/postgres"
	"github.com/pohcee/procy-server/server/storage/postgres/device"
	"github.com/pohcee/procy-server/server/storage/postgres/pgtest"
	"github.com/pohcee/procy-server/share/tenant"
)

// twoDeviceProcesses returns the DeviceTenants of two device processes on d (two device-role connection pools) and
// of a node with role "all" (application role), all keeping disconnected clients for keep.
func twoDeviceProcesses(t *testing.T, d *pgtest.DB, keep *time.Duration) (dev1, dev2 deviceTenants, all *DeviceTenants) {
	t.Helper()
	second, err := postgres.Open(context.Background(), d.DeviceURL)
	require.NoError(t, err)
	t.Cleanup(func() { second.Close() })
	return deviceTenants{db: d.Device, auth: device.ClientAuth{DB: d.Device}, keep: keep},
		deviceTenants{db: second, auth: device.ClientAuth{DB: second}, keep: keep},
		NewDeviceTenants(d.App, keep)
}

func admit(t *testing.T, dt chserver.DeviceTenants, tid tenant.ID, clientID string) chserver.Admission {
	t.Helper()
	a, err := dt.AdmitClient(context.Background(), tid, clientID)
	require.NoError(t, err)
	return a
}

func reportState(t *testing.T, owner *sqlx.DB, tid, clientID string, disconnectedAt *time.Time) {
	t.Helper()
	_, err := owner.Exec(`INSERT INTO client_state (tenant_id, client_id, client_auth_id, disconnected_at, details, reported_at)
		VALUES ($1, $2, 'auth', $3, '{}', now())`, tid, clientID, disconnectedAt)
	require.NoError(t, err)
}

// The client limit holds across device processes: a new client admitted by one process takes the place at once,
// before its state reaches the database, so another process refuses the next new one.
func TestAdmissionIsClusterWide(t *testing.T) {
	d := pgtest.New(t, "b")
	_, err := d.Owner.Exec("UPDATE tenants SET max_clients = 1 WHERE id = 'b'")
	require.NoError(t, err)
	dev1, dev2, all := twoDeviceProcesses(t, d, nil)

	assert.Equal(t, chserver.Admission{Status: "active", Clients: 1, MaxClients: 1}, admit(t, dev1, "b", "b1"))
	assert.Equal(t, chserver.Admission{Refused: chserver.RefusedLimit, Status: "active", Clients: 1, MaxClients: 1},
		admit(t, dev2, "b", "b3"), "the place b1 took on the other process")
	assert.Equal(t, chserver.RefusedLimit, admit(t, all, "b", "b3").Refused, "and on a node with role all")
	assert.Empty(t, admit(t, dev2, "b", "b1").Refused, "b1 connecting again elsewhere is the same client")

	// the admission lapses after 5 minutes; then the client's record counts (here: none, so the place is free)
	_, err = d.Owner.Exec("UPDATE client_admissions SET admitted_at = now() - interval '6 minutes'")
	require.NoError(t, err)
	assert.Empty(t, admit(t, dev2, "b", "b3").Refused)
}

// A client the tenant has (its record or reported state in the database, a live session) is admitted by any
// process, also with the tenant over its limit; a disconnected client kept no longer is not counted.
func TestAdmissionKnowsTheTenantsClientsFromTheDatabase(t *testing.T) {
	d := pgtest.New(t, "b")
	hour := time.Hour
	dev1, dev2, _ := twoDeviceProcesses(t, d, &hour)
	reportState(t, d.Owner, "b", "connected", nil)
	recent := time.Now().Add(-10 * time.Minute)
	reportState(t, d.Owner, "b", "recent", &recent)
	old := time.Now().Add(-2 * time.Hour)
	reportState(t, d.Owner, "b", "old", &old)
	_, err := d.Owner.Exec(`INSERT INTO client_sessions (tenant_id, client_id, node_id, session_id, lease_until)
		VALUES ('b', 'live', 'n1', 's-live', now() + interval '1 minute'), ('b', 'lapsed', 'n1', 's-lapsed', now() - interval '1 minute')`)
	require.NoError(t, err)
	_, err = d.Owner.Exec("UPDATE tenants SET max_clients = 2 WHERE id = 'b'")
	require.NoError(t, err)

	for _, id := range []string{"connected", "recent", "live"} {
		assert.Empty(t, admit(t, dev1, "b", id).Refused, "%s is the tenant's client", id)
	}
	a := admit(t, dev2, "b", "old")
	assert.Equal(t, chserver.RefusedLimit, a.Refused, "disconnected for longer than kept: new")
	assert.Equal(t, 3, a.Clients, "connected, recent and live count")
	assert.Equal(t, chserver.RefusedLimit, admit(t, dev2, "b", "lapsed").Refused)

	// without a keep_disconnected_clients rule every stored client counts
	_, _, all := twoDeviceProcesses(t, d, nil)
	assert.Empty(t, admit(t, all, "b", "old").Refused)
	a = admit(t, all, "b", "brand-new")
	assert.Equal(t, chserver.RefusedLimit, a.Refused)
	assert.Equal(t, 4, a.Clients)
}

// A client the tenant has is admitted without waiting for the tenant's admission lock (a reconnect storm after a
// failover doesn't queue behind new clients); a new client waits for it.
func TestAdmissionOfKnownClientTakesNoLock(t *testing.T) {
	d := pgtest.New(t, "b")
	_, err := d.Owner.Exec("UPDATE tenants SET max_clients = 5 WHERE id = 'b'")
	require.NoError(t, err)
	reportState(t, d.Owner, "b", "known", nil)
	dev1, _, _ := twoDeviceProcesses(t, d, nil)

	tx, err := d.Owner.Beginx()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended('rport/device-admission/' || 'b', 0))")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a, err := dev1.AdmitClient(ctx, "b", "known")
	require.NoError(t, err, "admitted while another admission holds the lock")
	assert.Empty(t, a.Refused)

	short, cancelShort := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancelShort()
	_, err = dev1.AdmitClient(short, "b", "new")
	assert.Error(t, err, "a new client waits for the lock")
}

// The lock serializes admissions only under READ COMMITTED; another isolation level is refused, not miscounted.
func TestAdmissionRefusesOtherIsolationLevels(t *testing.T) {
	d := pgtest.New(t, "b")
	_, err := d.Owner.Exec("UPDATE tenants SET max_clients = 1 WHERE id = 'b'")
	require.NoError(t, err)
	tx, err := d.Device.BeginTxx(context.Background(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var decision string
	err = tx.Get(&decision, "SELECT decision FROM device_admit_client('b', 'c1', NULL)")
	assert.ErrorContains(t, err, "needs READ COMMITTED")
}

// New clients of a limited tenant connecting to two processes at once: exactly max_clients are admitted.
func TestAdmissionConcurrentAcrossProcesses(t *testing.T) {
	d := pgtest.New(t, "c")
	_, err := d.Owner.Exec("UPDATE tenants SET max_clients = 3 WHERE id = 'c'")
	require.NoError(t, err)
	dev1, dev2, all := twoDeviceProcesses(t, d, nil)
	procs := []chserver.DeviceTenants{dev1, dev2, all}
	const n = 24
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a, err := procs[i%len(procs)].AdmitClient(context.Background(), "c", fmt.Sprintf("new-%d", i))
			assert.NoError(t, err)
			if err == nil && a.Refused == "" {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	assert.Equal(t, 3, admitted)
}

// The device role admits devices through device_admit_client and still reads no table.
func TestAdmissionDeviceRoleHasNoTableAccess(t *testing.T) {
	d := pgtest.New(t, "b")
	require.NoError(t, postgres.CheckDeviceRole(context.Background(), d.Device))
	for _, q := range []string{"SELECT count(*) FROM client_admissions", "SELECT count(*) FROM rport_counted_clients('b', NULL)"} {
		var n int
		assert.ErrorContains(t, d.Device.Get(&n, q), "permission denied", q)
	}
	_, err := d.App.Exec("INSERT INTO client_admissions (tenant_id, client_id) VALUES ('b', 'x')")
	assert.ErrorContains(t, err, "permission denied", "the application role reads it only")
}
