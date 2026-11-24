package chserver

import (
	"context"
	"fmt"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

// DeviceTenants tells a client listener shared by several tenants which tenant a connecting device belongs to and
// whether that tenant takes it (#34). Client auth ids are unique across the tenants, so the id a device logs in with
// names its tenant. Without DeviceTenants (standalone) every device belongs to the server's tenant, without limits.
type DeviceTenants interface {
	// TenantOfClientAuth returns the tenant holding the credentials with the client auth id, and false when no
	// tenant has them. ctx carries no tenant.
	TenantOfClientAuth(ctx context.Context, clientAuthID string) (tenant.ID, bool, error)
	// DevicePolicy returns what the tenant allows its devices.
	DevicePolicy(ctx context.Context, tid tenant.ID) (DevicePolicy, error)
}

// DevicePolicy is what a tenant allows its devices.
type DevicePolicy struct {
	// Status is the tenant's status; only an active tenant's devices connect.
	Status string
	// MaxClients is the most clients the tenant may have, when Limited. A client the tenant has already is never
	// refused; a new one is refused once the tenant has MaxClients (non-obsolete) clients.
	MaxClients int
	Limited    bool
}

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

// deviceAdmission serializes the admission of new clients per tenant, so two new devices of a limited tenant
// connecting at once can't both take its last place.
type deviceAdmission struct {
	mu    sync.Mutex
	locks map[tenant.ID]*sync.Mutex
}

func (a *deviceAdmission) lock(tid tenant.ID) func() {
	a.mu.Lock()
	if a.locks == nil {
		a.locks = map[tenant.ID]*sync.Mutex{}
	}
	m := a.locks[tid]
	if m == nil {
		m = &sync.Mutex{}
		a.locks[tid] = m
	}
	a.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// admitDevice decides whether the device clientID (logged in with clientAuthID) may connect to its tenant tid (ctx
// carries it). It returns the error to answer the connection request with, which the client logs, and, when the
// admission holds the tenant's lock, the function releasing it once the client is started. Without DeviceTenants
// every device is admitted.
func (cl *ClientListener) admitDevice(ctx context.Context, tid tenant.ID, clientID, clientAuthID string, clog *logger.Logger) (release func(), err error) {
	release = func() {}
	dt := cl.server.deviceTenants
	if dt == nil || tid == "" {
		return release, nil
	}
	policy, err := dt.DevicePolicy(ctx, tid)
	if err != nil {
		clog.Errorf("client %s (client auth id %q): reading the policy of tenant %q failed: %v", clientID, clientAuthID, tid, err)
		return release, fmt.Errorf("connection refused: the server could not check the organization of this client; try again later")
	}
	if policy.Status != TenantStatusActive {
		clog.Infof("refused client %s (client auth id %q): tenant %q is %s", clientID, clientAuthID, tid, policy.Status)
		return release, fmt.Errorf("connection refused: the organization of this client is %s", policy.Status)
	}
	if !policy.Limited {
		return release, nil
	}
	known, err := cl.getClientService().GetByID(ctx, clientID)
	if err != nil {
		return release, fmt.Errorf("could not get client %q: %w", clientID, err)
	}
	if known != nil {
		// a client the tenant has already is never refused
		return release, nil
	}
	unlock := cl.admission.lock(tid)
	// checked again under the lock: another connection of the same new client may have been admitted meanwhile
	if known, err = cl.getClientService().GetByID(ctx, clientID); err != nil || known != nil {
		unlock()
		if err != nil {
			return release, fmt.Errorf("could not get client %q: %w", clientID, err)
		}
		return release, nil
	}
	if n := cl.getClientService().Count(ctx); n >= policy.MaxClients {
		unlock()
		clog.Infof("refused new client %s (client auth id %q): tenant %q has %d of its %d clients", clientID, clientAuthID, tid, n, policy.MaxClients)
		return release, fmt.Errorf("connection refused: the organization's limit of %d clients is reached", policy.MaxClients)
	}
	return unlock, nil
}
