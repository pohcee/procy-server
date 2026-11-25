package pgserver

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	chserver "github.com/openrport/openrport/server"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/server/storage/postgres/clientauth"
	"github.com/openrport/openrport/share/tenant"
)

// DeviceTenants lets devices of every tenant connect to a node: the tenant of a device is the one holding its client
// auth id (unique across tenants), and the database decides whether the tenant takes the device
// (device_admit_client: the tenant's status and max_clients, its clients counted across the cluster).
type DeviceTenants struct {
	db   *sqlx.DB
	auth *clientauth.Store
	keep *time.Duration
}

var _ chserver.DeviceTenants = (*DeviceTenants)(nil)

// NewDeviceTenants returns the DeviceTenants of the database db (migrated, application role). keep is how long
// disconnected clients are kept (nil: forever), as for the clients store: a client disconnected for longer is not
// counted.
func NewDeviceTenants(db *sqlx.DB, keep *time.Duration) *DeviceTenants {
	return &DeviceTenants{db: db, auth: clientauth.New(db), keep: keep}
}

// TenantOfClientAuth returns the tenant holding the client auth id; it needs no tenant in ctx.
func (d *DeviceTenants) TenantOfClientAuth(ctx context.Context, clientAuthID string) (tenant.ID, bool, error) {
	return d.auth.TenantOf(ctx, clientAuthID)
}

// AdmitClient asks the database whether tenant tid takes the device clientID.
func (d *DeviceTenants) AdmitClient(ctx context.Context, tid tenant.ID, clientID string) (chserver.Admission, error) {
	return admitClient(ctx, d.db, tid, clientID, d.keep)
}

// admitClient calls device_admit_client (migration 1350), which the application role and the device role may
// execute. keep is the keep_disconnected_clients rule (nil: disconnected clients are kept, and counted, forever).
func admitClient(ctx context.Context, db *sqlx.DB, tid tenant.ID, clientID string, keep *time.Duration) (chserver.Admission, error) {
	var cutoff *time.Time
	if keep != nil {
		c := clientdata.Now().Add(-*keep)
		cutoff = &c
	}
	var row struct {
		Decision   string        `db:"decision"`
		Status     string        `db:"tenant_status"`
		Clients    int           `db:"clients"`
		MaxClients sql.NullInt64 `db:"max_clients"`
	}
	err := db.GetContext(ctx, &row, "SELECT decision, tenant_status, clients, max_clients FROM device_admit_client($1, $2, $3)",
		string(tid), clientID, cutoff)
	if err != nil {
		return chserver.Admission{}, fmt.Errorf("admit client %q to tenant %q: %w", clientID, tid, err)
	}
	a := chserver.Admission{Status: row.Status, Clients: row.Clients, MaxClients: int(row.MaxClients.Int64)}
	switch row.Decision {
	case "admitted":
	case "refused_limit":
		a.Refused = chserver.RefusedLimit
	case "refused_status":
		a.Refused = chserver.RefusedStatus
	default:
		return chserver.Admission{}, fmt.Errorf("admit client %q to tenant %q: unknown decision %q", clientID, tid, row.Decision)
	}
	return a, nil
}

// Tenants returns every tenant that is not reserved and can hold devices (not _system or another id starting
// with "_"), in id order.
func (d *DeviceTenants) Tenants(ctx context.Context) ([]tenant.ID, error) {
	var ids []string
	if err := d.db.SelectContext(ctx, &ids, "SELECT id FROM tenants WHERE status <> 'reserved' AND left(id, 1) <> '_' ORDER BY id"); err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	out := make([]tenant.ID, len(ids))
	for i, id := range ids {
		out[i] = tenant.ID(id)
	}
	return out, nil
}
