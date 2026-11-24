package pgserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"

	chserver "github.com/openrport/openrport/server"
	"github.com/openrport/openrport/server/storage/postgres/clientauth"
	"github.com/openrport/openrport/share/tenant"
)

// DeviceTenants lets devices of every tenant connect to a node: the tenant of a device is the one holding its client
// auth id (unique across tenants), and the tenant's status and max_clients (tenants table) decide whether it takes
// the device.
type DeviceTenants struct {
	db   *sqlx.DB
	auth *clientauth.Store
}

var _ chserver.DeviceTenants = (*DeviceTenants)(nil)

// NewDeviceTenants returns the DeviceTenants of the database db (migrated, application role).
func NewDeviceTenants(db *sqlx.DB) *DeviceTenants {
	return &DeviceTenants{db: db, auth: clientauth.New(db)}
}

// TenantOfClientAuth returns the tenant holding the client auth id; it needs no tenant in ctx.
func (d *DeviceTenants) TenantOfClientAuth(ctx context.Context, clientAuthID string) (tenant.ID, bool, error) {
	return d.auth.TenantOf(ctx, clientAuthID)
}

// DevicePolicy reads the tenant's status and client limit. A tenant that does not exist (deleted meanwhile) has
// the status "deleted", so its devices are refused.
func (d *DeviceTenants) DevicePolicy(ctx context.Context, tid tenant.ID) (chserver.DevicePolicy, error) {
	var row struct {
		Status     string        `db:"status"`
		MaxClients sql.NullInt64 `db:"max_clients"`
	}
	err := d.db.GetContext(ctx, &row, "SELECT status, max_clients FROM tenants WHERE id = $1", string(tid))
	if errors.Is(err, sql.ErrNoRows) {
		return chserver.DevicePolicy{Status: "deleted"}, nil
	}
	if err != nil {
		return chserver.DevicePolicy{}, fmt.Errorf("device policy of tenant %q: %w", tid, err)
	}
	return chserver.DevicePolicy{
		Status:     row.Status,
		Limited:    row.MaxClients.Valid,
		MaxClients: int(row.MaxClients.Int64),
	}, nil
}

// Tenants returns every tenant that is not reserved, in id order.
func (d *DeviceTenants) Tenants(ctx context.Context) ([]tenant.ID, error) {
	var ids []string
	if err := d.db.SelectContext(ctx, &ids, "SELECT id FROM tenants WHERE status <> 'reserved' ORDER BY id"); err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	out := make([]tenant.ID, len(ids))
	for i, id := range ids {
		out[i] = tenant.ID(id)
	}
	return out, nil
}
