package chserver

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/crypto/ssh"

	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/tenant"
)

// DeviceTenants tells a client listener shared by several tenants which tenant a connecting device belongs to and
// whether that tenant takes it (#34). Client auth ids are unique across the tenants, so the id a device logs in with
// names its tenant. Without DeviceTenants (standalone) every device belongs to the server's tenant, without limits.
type DeviceTenants interface {
	// TenantOfClientAuth returns the tenant holding the credentials with the client auth id, and false when no
	// tenant has them. It looks across all tenants: ctx may carry the listener's own tenant, which it must ignore.
	TenantOfClientAuth(ctx context.Context, clientAuthID string) (tenant.ID, bool, error)
	// AdmitClient decides whether the tenant takes the device clientID now connecting: only an active tenant takes
	// devices, a client the tenant has already is never refused, and a new one only while the tenant has fewer
	// clients than its limit, counted across every process accepting devices (an admitted new client is counted
	// from then on).
	AdmitClient(ctx context.Context, tid tenant.ID, clientID string) (Admission, error)
	// Tenants returns every tenant that can have devices (not the reserved ones). The listener loads their clients
	// when it starts, so clients stored as connected by an earlier run are marked disconnected.
	Tenants(ctx context.Context) ([]tenant.ID, error)
}

// Admission is a tenant's answer to a connecting device.
type Admission struct {
	// Refused is why the device is refused (RefusedStatus, RefusedLimit), or "" when it is admitted.
	Refused string
	// Status is the tenant's status ("deleted" when the tenant doesn't exist).
	Status string
	// Clients is the number of clients the tenant has, MaxClients its limit (with RefusedLimit).
	Clients    int
	MaxClients int
}

// Reasons of a refused Admission.
const (
	// RefusedStatus: the tenant is not active (Admission.Status).
	RefusedStatus = "status"
	// RefusedLimit: a new client of a tenant that has Admission.MaxClients clients.
	RefusedLimit = "limit"
)

// TenantStatusActive is the status of a tenant whose devices connect.
const TenantStatusActive = "active"

// permTenant is the ssh.Permissions extension that carries the tenant of an authenticated device from the password
// callback to the connection handler.
const permTenant = "rport-tenant"

// deviceTenantOf returns the tenant authUser found for the connection, or "" when it found none (standalone).
func deviceTenantOf(conn *ssh.ServerConn) tenant.ID {
	if conn == nil || conn.Permissions == nil {
		return ""
	}
	return tenant.ID(conn.Permissions.Extensions[permTenant])
}

// loadDeviceTenants loads the clients of every tenant before the listener accepts devices: loading marks the
// clients stored as connected (by an earlier run of this listener) disconnected, which the server's own tenant gets
// at startup and the other tenants would otherwise only get when one of their devices connects. Errors are logged;
// a tenant that failed is loaded when it is next used.
func (cl *ClientListener) loadDeviceTenants(ctx context.Context) {
	dt := cl.server.deviceTenants
	if dt == nil {
		return
	}
	tids, err := dt.Tenants(ctx)
	if err != nil {
		cl.log().Errorf("listing the tenants to load their clients failed: %v", err)
		return
	}
	repo := cl.getClientService().GetRepo()
	for _, tid := range tids {
		if err := repo.LoadTenant(tenant.WithID(ctx, tid)); err != nil {
			cl.log().Errorf("%v", err)
		}
	}
}

// admitDevice decides whether the device clientID (logged in with clientAuthID) may connect to its tenant tid (ctx
// carries it) and returns the error to answer the connection request with, which the client logs. The tenant's
// decision comes from DeviceTenants (the database in a cluster), never from the clients this process holds: a client
// connected to another process counts, and a client the tenant has is known wherever it connected before. Without
// DeviceTenants every device is admitted.
func (cl *ClientListener) admitDevice(ctx context.Context, tid tenant.ID, clientID, clientAuthID string, clog *logger.Logger) error {
	dt := cl.server.deviceTenants
	if dt == nil {
		return nil
	}
	if tid == "" {
		// authUser always finds the tenant when the listener serves several; never run such a device in the
		// listener's own tenant
		clog.Errorf("refused client %s (client auth id %q): no tenant", clientID, clientAuthID)
		return errUnknownTenant
	}
	a, err := dt.AdmitClient(ctx, tid, clientID)
	if err != nil {
		clog.Errorf("client %s (client auth id %q): admission to tenant %q failed: %v", clientID, clientAuthID, tid, err)
		return fmt.Errorf("connection refused: the server could not check the organization of this client; try again later")
	}
	switch a.Refused {
	case "":
		return nil
	case RefusedLimit:
		clog.Infof("refused new client %s (client auth id %q): tenant %q has %d of its %d clients", clientID, clientAuthID, tid, a.Clients, a.MaxClients)
		return fmt.Errorf("%s: the organization's limit of %d clients is reached", refusedPrefix, a.MaxClients)
	default:
		clog.Infof("refused client %s (client auth id %q): tenant %q is %s", clientID, clientAuthID, tid, a.Status)
		return fmt.Errorf("%s: the organization of this client is %s", refusedPrefix, a.Status)
	}
}

// refusedPrefix starts the reply to a device its organization refuses; the client waits longer before it retries
// after such a reply (client.organizationRefusal).
const refusedPrefix = "connection refused"

var errUnknownTenant = errors.New(refusedPrefix + ": the organization of this client is unknown")
