package monitoring

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	monitoringmigration "github.com/openrport/openrport/db/migration/monitoring"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
)

func ctxFor(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

// migrateTo opens a file DB and migrates it to the given schema version (up or down).
func migrateTo(t *testing.T, path string, version uint) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("sqlite3", path)
	require.NoError(t, err)
	require.NoError(t, sqlite.MigrateTo(db, monitoringmigration.FS, version))
	return db
}

func tenantMeasurement(clientID string, ts time.Time, cpu float64) *models.Measurement {
	return &models.Measurement{
		ClientID:        clientID,
		Timestamp:       ts,
		CPUUsagePercent: cpu,
		Processes:       "[]",
		Mountpoints:     "{}",
	}
}

func TestMeasurementsRequireTenant(t *testing.T) {
	p, err := openTestSqliteProvider(":memory:", DataSourceOptions, testLog)
	require.NoError(t, err)
	defer p.Close()
	ctx := context.Background()

	assert.ErrorIs(t, p.CreateMeasurement(ctx, tenantMeasurement("c1", measurement1, 1)), tenant.ErrMissing)
	_, err = p.DeleteMeasurementsBefore(ctx, measurement3)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.CountByClientID(ctx, "c1", createMetricsDefaultOptions())
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.ListMetricsByClientID(ctx, "c1", createMetricsDefaultOptions())
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.ListProcessesByClientID(ctx, "c1", createProcessesDefaultOptions())
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.ListMountpointsByClientID(ctx, "c1", createMountpointsDefaultOptions())
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.ListGraphMetricsByClientID(ctx, "c1", 2, createGraphMetricsDefaultOptions(measurement1, 2, layoutDb))
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = p.ListGraphByClientID(ctx, "c1", 2, createGraphMetricsDefaultOptions(measurement1, 2, layoutDb), "cpu_usage_percent")
	assert.ErrorIs(t, err, tenant.ErrMissing)
}

func TestMeasurementsIsolateTenants(t *testing.T) {
	p, err := openTestSqliteProvider(":memory:", DataSourceOptions, testLog)
	require.NoError(t, err)
	defer p.Close()
	a, b := ctxFor("a"), ctxFor("b")

	// the same client ID can exist in both tenants (the key stays (client_id, timestamp), see
	// TestUpgradeAddsTenantColumnInPlace, so the timestamps differ)
	require.NoError(t, p.CreateMeasurement(a, tenantMeasurement("c1", measurement1, 10)))
	require.NoError(t, p.CreateMeasurement(a, tenantMeasurement("c1", measurement2, 11)))
	require.NoError(t, p.CreateMeasurement(b, tenantMeasurement("c1", measurement1.Add(time.Second), 20)))

	count, err := p.CountByClientID(a, "c1", createMetricsDefaultOptions())
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	count, err = p.CountByClientID(b, "c1", createMetricsDefaultOptions())
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	metrics, err := p.ListMetricsByClientID(b, "c1", createMetricsDefaultOptions())
	require.NoError(t, err)
	require.Len(t, metrics, 1)
	assert.Equal(t, 20.0, metrics[0].CPUUsagePercent)

	procs, err := p.ListProcessesByClientID(b, "c1", &query.ListOptions{Fields: query.ParseFieldsOptions(ClientProcessesFieldsDefault)})
	require.NoError(t, err)
	assert.Len(t, procs, 1)
	mps, err := p.ListMountpointsByClientID(b, "c1", &query.ListOptions{Fields: query.ParseFieldsOptions(ClientMountpointsFieldsDefault)})
	require.NoError(t, err)
	assert.Len(t, mps, 1)

	graph, err := p.ListGraphMetricsByClientID(b, "c1", 2, createGraphMetricsDefaultOptions(measurement1.Add(-time.Minute), 2, layoutDb))
	require.NoError(t, err)
	require.Len(t, graph, 1)
	assert.Equal(t, 20.0, graph[0].CPUUsagePercent.Avg)

	g, err := p.ListGraphByClientID(b, "c1", 2, createGraphMetricsDefaultOptions(measurement1.Add(-time.Minute), 2, layoutDb), "cpu_usage_percent")
	require.NoError(t, err)
	require.Len(t, g, 1)
	assert.Equal(t, 20.0, g[0].CPUUsagePercent.Avg)

	// deleting in tenant a leaves tenant b alone
	deleted, err := p.DeleteMeasurementsBefore(a, measurement3)
	require.NoError(t, err)
	assert.Equal(t, int64(2), deleted)
	count, err = p.CountByClientID(b, "c1", createMetricsDefaultOptions())
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestUpgradeKeepsMeasurementsInDefaultTenant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitoring.db")
	old := migrateTo(t, path, 3)
	_, err := old.Exec(`INSERT INTO measurements (client_id, timestamp, cpu_usage_percent, memory_usage_percent, io_usage_percent, processes, mountpoints)
		VALUES ('legacy', ?, 42, 0, 0, '[]', '{}')`, measurement1)
	require.NoError(t, err)
	require.NoError(t, old.Close())

	p, err := openTestSqliteProvider(path, DataSourceOptions, testLog)
	require.NoError(t, err)
	defer p.Close()

	metrics, err := p.ListMetricsByClientID(ctxFor(tenant.Default), "legacy", createMetricsDefaultOptions())
	require.NoError(t, err)
	require.Len(t, metrics, 1)
	assert.Equal(t, 42.0, metrics[0].CPUUsagePercent)
}

func TestDowngradeKeepsOnlyDefaultMeasurements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitoring.db")
	p, err := openTestSqliteProvider(path, DataSourceOptions, testLog)
	require.NoError(t, err)
	require.NoError(t, p.CreateMeasurement(ctxFor(tenant.Default), tenantMeasurement("c1", measurement1, 1)))
	require.NoError(t, p.CreateMeasurement(ctxFor("other"), tenantMeasurement("c1", measurement1.Add(time.Second), 2)))
	require.NoError(t, p.Close())

	db := migrateTo(t, path, 3)
	defer db.Close()
	var cpus []float64
	require.NoError(t, db.Select(&cpus, "SELECT cpu_usage_percent FROM measurements"))
	assert.Equal(t, []float64{1}, cpus)
}

// Measurements can be several GB: the upgrade adds the tenant column in place instead of copying the
// table (which needs as much free space again and takes long). The key stays (client_id, timestamp).
func TestUpgradeAddsTenantColumnInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitoring.db")
	old := migrateTo(t, path, 3)
	_, err := old.Exec(`INSERT INTO measurements (client_id, timestamp, cpu_usage_percent, memory_usage_percent, io_usage_percent, processes, mountpoints)
		VALUES ('legacy', ?, 42, 0, 0, '[]', '{}')`, measurement1)
	require.NoError(t, err)
	var rootBefore int
	require.NoError(t, old.Get(&rootBefore, "SELECT rootpage FROM sqlite_master WHERE type='table' AND name='measurements'"))
	require.NoError(t, old.Close())

	db := migrateTo(t, path, 4)
	defer db.Close()
	var rootAfter int
	require.NoError(t, db.Get(&rootAfter, "SELECT rootpage FROM sqlite_master WHERE type='table' AND name='measurements'"))
	assert.Equal(t, rootBefore, rootAfter, "the table is not rebuilt")
	var tid string
	require.NoError(t, db.Get(&tid, "SELECT tenant_id FROM measurements WHERE client_id='legacy'"))
	assert.Equal(t, "default", tid)
}
