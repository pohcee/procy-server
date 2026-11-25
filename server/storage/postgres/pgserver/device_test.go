package pgserver

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/storage/postgres/device"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	"github.com/openrport/openrport/share/tenant"
)

func deviceNodeConf(t *testing.T, dsn, apiAddr string) string {
	t.Helper()
	return fmt.Sprintf(`
[server]
  address = %q
  data_dir = %q
[logging]
  log_level = "error"
[api]
  address = %q
[storage]
  backend = "postgres"
  dsn = %q
  role = "device"
[cluster]
  node_id = "dev-1"
`, freeAddr(t), t.TempDir(), apiAddr, dsn)
}

func TestServerOptsRoleDevice(t *testing.T) {
	d := pgtest.New(t)
	ctx := context.Background()
	cfg := loadConfig(t, deviceNodeConf(t, d.DeviceURL, ""))
	opts, release, err := NodeOpts(ctx, cfg, d.Device, nil, io.Discard)
	require.NoError(t, err)
	defer release()
	assert.False(t, opts.DisableClientListener, "a device process accepts devices")
	assert.Nil(t, opts.Dispatcher, "and runs the work on them")
	assert.IsType(t, device.ClientAuth{}, opts.Repositories.ClientAuth)
	assert.IsType(t, &device.Clients{}, opts.Repositories.Clients)
	assert.Nil(t, opts.VaultKeys, "no vault")
	coord, ok := opts.Cluster.(*DeviceCoordinator)
	require.True(t, ok)
	held, err := coord.Lease(ctx, "jobs-cleanup", "default", time.Minute)
	require.NoError(t, err)
	assert.False(t, held, "background work stays with the api processes")
	claimed, err := coord.ClaimRun(ctx, "s1", time.Now())
	require.NoError(t, err)
	assert.False(t, claimed)
	tenants, err := coord.Tenants(ctx)
	require.NoError(t, err)
	assert.Contains(t, tenants, tenant.Default)
	_, err = os.Stat(filepath.Join(cfg.Server.DataDir, OutboxFile))
	assert.NoError(t, err, "the outbox is in the data directory")
}

// A device process refuses a login with more privilege than the device role: the application role, the owner.
func TestDeviceRoleRefusesOverPrivilegedLogin(t *testing.T) {
	d := pgtest.New(t)
	for _, c := range []struct{ name, url, want string }{
		{"application role", d.AppURL, "more than the device role needs"},
		{"owner", d.OwnerURL, "row-level security"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := loadConfig(t, deviceNodeConf(t, c.url, ""))
			db, err := Open(context.Background(), cfg)
			require.NoError(t, err)
			defer db.Close()
			_, _, err = NodeOpts(context.Background(), cfg, db, nil, io.Discard)
			assert.ErrorContains(t, err, c.want)
		})
	}
}

func TestDeviceRoleRefusesAPIAddress(t *testing.T) {
	d := pgtest.New(t)
	_, err := parseConfig(t, deviceNodeConf(t, d.DeviceURL, freeAddr(t)))
	assert.ErrorContains(t, err, "[api] address must be empty")
}

// A device process with [billing] on reads the transfer policies through the device role; the api processes run
// the upkeep and serve the API.
func TestServerOptsRoleDeviceBilling(t *testing.T) {
	d := pgtest.New(t)
	ctx := context.Background()
	cfg := loadConfig(t, deviceNodeConf(t, d.DeviceURL, "")+billingOn(t))
	opts, release, err := NodeOpts(ctx, cfg, d.Device, nil, io.Discard)
	require.NoError(t, err)
	defer release()
	require.NotNil(t, opts.Billing)
	assert.Nil(t, opts.Billing.Upkeep)
	assert.Nil(t, opts.Billing.Org)
	_, err = opts.Billing.Policies.TransferPolicies(ctx)
	assert.NoError(t, err)

	_, err = d.Owner.ExecContext(ctx, "REVOKE EXECUTE ON FUNCTION device_transfer_policy() FROM "+pgtest.DeviceRole)
	require.NoError(t, err)
	_, _, err = NodeOpts(ctx, cfg, d.Device, nil, io.Discard)
	assert.ErrorContains(t, err, "device_transfer_policy")
}
