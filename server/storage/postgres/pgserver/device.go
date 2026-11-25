package pgserver

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	chserver "github.com/openrport/openrport/server"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/cluster"
	"github.com/openrport/openrport/server/storage/outbox"
	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/device"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

// OutboxFile is the outbox of a device process, in its data directory.
const OutboxFile = "outbox.db"

// DeviceCoordinator is the cluster.Coordinator of a device process. Its database role has no access to the change
// feed, leases or schedule claims, so it never runs background work or scheduled runs (the api processes do), sees
// no changes, and its Run ships the outbox.
type DeviceCoordinator struct {
	db      *sqlx.DB
	nodeID  string
	shipper *outbox.Shipper
}

var _ cluster.Coordinator = (*DeviceCoordinator)(nil)

func (c *DeviceCoordinator) NodeID() string { return c.nodeID }

// Tenants returns the active tenants (device_tenants).
func (c *DeviceCoordinator) Tenants(ctx context.Context) ([]tenant.ID, error) {
	var rows []struct {
		ID     string `db:"id"`
		Status string `db:"status"`
	}
	if err := c.db.SelectContext(ctx, &rows, "SELECT id, status FROM device_tenants()"); err != nil {
		return nil, fmt.Errorf("cluster: tenants: %w", err)
	}
	var out []tenant.ID
	for _, r := range rows {
		if r.Status == "active" {
			out = append(out, tenant.ID(r.ID))
		}
	}
	return out, nil
}

// Lease is never held: background work runs on the api processes.
func (c *DeviceCoordinator) Lease(context.Context, string, tenant.ID, time.Duration) (bool, error) {
	return false, nil
}

// ClaimRun never claims: scheduled runs start on the api processes.
func (c *DeviceCoordinator) ClaimRun(context.Context, string, time.Time) (bool, error) {
	return false, nil
}

// Watch does nothing: a device process caches nothing that the feed would invalidate.
func (c *DeviceCoordinator) Watch(cluster.Watcher) {}

// Run ships the outbox until ctx is done.
func (c *DeviceCoordinator) Run(ctx context.Context) error {
	c.shipper.Run(ctx)
	return nil
}

// Clustered is true.
func (c *DeviceCoordinator) Clustered() bool { return true }

// deviceTenants is the chserver.DeviceTenants of a device process: the tenant of a device from device_client_auth,
// the tenants' status and client limit from device_tenants.
type deviceTenants struct {
	db   *sqlx.DB
	auth device.ClientAuth
}

var _ chserver.DeviceTenants = deviceTenants{}

func (d deviceTenants) TenantOfClientAuth(ctx context.Context, clientAuthID string) (tenant.ID, bool, error) {
	tid, _, found, err := d.auth.Lookup(ctx, clientAuthID)
	return tid, found, err
}

type deviceTenantRow struct {
	ID         string        `db:"id"`
	Status     string        `db:"status"`
	MaxClients sql.NullInt64 `db:"max_clients"`
}

func (d deviceTenants) rows(ctx context.Context) ([]deviceTenantRow, error) {
	var rows []deviceTenantRow
	if err := d.db.SelectContext(ctx, &rows, "SELECT id, status, max_clients FROM device_tenants()"); err != nil {
		return nil, fmt.Errorf("device tenants: %w", err)
	}
	return rows, nil
}

// DevicePolicy is the tenant's status and client limit; a tenant that doesn't exist has the status "deleted".
func (d deviceTenants) DevicePolicy(ctx context.Context, tid tenant.ID) (chserver.DevicePolicy, error) {
	rows, err := d.rows(ctx)
	if err != nil {
		return chserver.DevicePolicy{}, err
	}
	for _, r := range rows {
		if r.ID == string(tid) {
			return chserver.DevicePolicy{Status: r.Status, Limited: r.MaxClients.Valid, MaxClients: int(r.MaxClients.Int64)}, nil
		}
	}
	return chserver.DevicePolicy{Status: "deleted"}, nil
}

// Tenants are the tenants that can have devices (not reserved, no id starting with "_").
func (d deviceTenants) Tenants(ctx context.Context) ([]tenant.ID, error) {
	rows, err := d.rows(ctx)
	if err != nil {
		return nil, err
	}
	var out []tenant.ID
	for _, r := range rows {
		if r.Status != "reserved" && !strings.HasPrefix(r.ID, "_") {
			out = append(out, tenant.ID(r.ID))
		}
	}
	return out, nil
}

// deviceNodeOpts fills opts for a node with [storage] role = "device": the device path on the device role's
// functions, what devices report through the outbox in the data directory, no API listener (the config refuses an
// [api] address), no vault. The repositories a device process has no use for (library, schedules, notifications,
// API tokens and sessions, vault) are left to the server's local SQLite files in the data directory, which stay
// empty: nothing on a device process writes them.
func deviceNodeOpts(ctx context.Context, cfg *chconfig.Config, db *sqlx.DB, opts *chserver.ServerOpts) (release func(), _ error) {
	// the device role can't read schema_migrations: the functions it needs must exist and be granted, and nothing
	// more may be
	if err := postgres.CheckDeviceRole(ctx, db); err != nil {
		return nil, err
	}
	if cfg.Metering.Enabled {
		if err := postgres.CheckDeviceMetering(ctx, db); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(cfg.Server.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("device: data dir: %w", err)
	}
	ob, err := outbox.Open(filepath.Join(cfg.Server.DataDir, OutboxFile), outbox.Options{})
	if err != nil {
		return nil, err
	}
	log := logger.NewLogger("outbox", cfg.Logging.LogOutput, cfg.Logging.LogLevel)
	opts.Repositories = &chserver.Repositories{
		Clients:       &device.Clients{DB: db, Outbox: ob, NodeID: cfg.Cluster.NodeID},
		StoredTunnels: device.StoredTunnels{DB: db},
		ClientGroups:  device.ClientGroups{DB: db},
		Jobs:          device.Jobs{Outbox: ob},
		Monitoring:    device.Monitoring{Outbox: ob},
		AuditLog:      device.AuditLog{Outbox: ob},
		Users:         device.NoUsers{},
		ClientAuth:    device.ClientAuth{DB: db},
	}
	if cfg.Metering.Enabled {
		// what this process counts is shipped with the rest of the outbox; the api processes serve the usage API
		opts.Metering = &chserver.MeteringOpts{Writer: device.Usage{Outbox: ob}}
	}
	opts.SkipMigrations = false
	coord := &DeviceCoordinator{db: db, nodeID: cfg.Cluster.NodeID,
		shipper: &outbox.Shipper{Outbox: ob, Sink: device.Sink{DB: db}, Log: log}}
	opts.Cluster = coord
	// devices of every tenant connect; their client auth id names the tenant
	opts.DeviceTenants = deviceTenants{db: db, auth: device.ClientAuth{DB: db}}
	// the session registry, and with peer calls the peer listener api processes dispatch to
	cp, err := newClusterProcess(cfg, db, coord, opts)
	if err != nil {
		_ = ob.Close()
		return nil, err
	}
	opts.Peer = cp
	return func() { _ = ob.Close() }, nil
}
