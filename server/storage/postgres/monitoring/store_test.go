package monitoring_test

import (
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	oss "github.com/openrport/openrport/server/monitoring"
	"github.com/openrport/openrport/server/repotest"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/tenant"

	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/monitoring"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestContract(t *testing.T) {
	repotest.Monitoring(t, func(t *testing.T) oss.DBProvider {
		d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
		return monitoring.New(d.App)
	})
}

var t0 = time.Date(2021, 9, 1, 0, 0, 0, 0, time.UTC)

func seed(t *testing.T, d *pgtest.DB) {
	t.Helper()
	s := monitoring.New(d.App)
	for _, id := range []tenant.ID{repotest.TenantA, repotest.TenantB} {
		require.NoError(t, s.CreateMeasurement(repotest.Ctx(id), &models.Measurement{ClientID: "c1", Timestamp: t0, Processes: "[]", Mountpoints: "{}"}))
	}
}

func TestRLSBackstop(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	seed(t, d)
	var tenants []string
	require.NoError(t, postgres.InTenant(repotest.Ctx(repotest.TenantA), d.App, func(tx *sqlx.Tx) error {
		return tx.Select(&tenants, "SELECT tenant_id FROM measurements")
	}))
	assert.Equal(t, []string{"a"}, tenants)
	var n int
	require.NoError(t, d.App.Get(&n, "SELECT count(*) FROM measurements"))
	assert.Zero(t, n)
}

func TestTenantDeleteCascades(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	seed(t, d)
	_, err := d.Owner.Exec("DELETE FROM tenants WHERE id = 'a'")
	require.NoError(t, err)
	var tenants []string
	require.NoError(t, d.Owner.Select(&tenants, "SELECT tenant_id FROM measurements"))
	assert.Equal(t, []string{"b"}, tenants)
}

// Unlike SQLite (key client_id, timestamp), two tenants' clients of the same ID may measure at the same time.
func TestSameClientAndTimeInTwoTenants(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	seed(t, d)
	var n int
	require.NoError(t, d.Owner.Get(&n, "SELECT count(*) FROM measurements WHERE client_id = 'c1' AND timestamp = $1", t0))
	assert.Equal(t, 2, n)
}

// The cleanup deletes in chunks of MaxDeletedEntries distinct times.
func TestDeleteInChunks(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	_, err := d.Owner.Exec(`INSERT INTO measurements (tenant_id, client_id, timestamp, cpu_usage_percent, memory_usage_percent, io_usage_percent)
		SELECT 'a', 'c' || (i % 2), $1::timestamptz + (i / 2) * interval '1 second', 0, 0, 0 FROM generate_series(0, 2 * $2 + 9) AS i`, t0, monitoring.MaxDeletedEntries)
	require.NoError(t, err)
	s := monitoring.New(d.App)
	ctx := repotest.Ctx(repotest.TenantA)
	deleted, err := s.DeleteMeasurementsBefore(ctx, t0.Add(24*time.Hour))
	require.NoError(t, err)
	assert.EqualValues(t, 2*monitoring.MaxDeletedEntries, deleted)
	deleted, err = s.DeleteMeasurementsBefore(ctx, t0.Add(24*time.Hour))
	require.NoError(t, err)
	assert.EqualValues(t, 10, deleted)
}
