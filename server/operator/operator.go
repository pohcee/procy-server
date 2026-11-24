// Package operator is the platform operator API of a server on Postgres: platform operators (the administrators of
// the reserved system tenant) list, create, suspend and reactivate organizations (tenants), set their device limits
// and create their first administrators. The storage lives in pgserver; standalone servers have no operator API.
package operator

import (
	"context"
	"errors"
	"time"

	"github.com/openrport/openrport/share/tenant"
)

// SystemTenant is the reserved tenant of the platform operators. It has users but never devices, and through the
// normal API its users see only their own tenant.
const SystemTenant tenant.ID = "_system"

// Organization statuses an operator can set.
const (
	StatusActive    = "active"
	StatusSuspended = "suspended"
)

// Audit log applications and actions of the operator API, written in the system tenant.
const (
	AuditApplicationOrganization      = "operator.organization"
	AuditApplicationOrganizationAdmin = "operator.organization.admin"
	AuditActionCreate                 = "create"
	AuditActionSuspend                = "suspend"
	AuditActionActivate               = "activate"
	AuditActionUpdate                 = "update"
)

var (
	// ErrNotFound: no such organization (or a reserved one).
	ErrNotFound = errors.New("organization not found")
	// ErrExists: the organization id or the username is taken.
	ErrExists = errors.New("already exists")
	// ErrInvalid: the input is not valid; wrapped with the reason.
	ErrInvalid = errors.New("invalid input")
	// ErrNotActive: the organization is suspended.
	ErrNotActive = errors.New("organization is not active")
)

// Organization is a tenant as operators see it.
type Organization struct {
	ID        string    `json:"id" db:"id"`
	Name      string    `json:"name" db:"name"`
	Status    string    `json:"status" db:"status"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
	// DeviceCount is the number of clients (devices) the organization has.
	DeviceCount int `json:"device_count" db:"-"`
	// DeviceLimit is the most devices it may have; nil is unlimited.
	DeviceLimit *int `json:"device_limit" db:"max_clients"`
	UserCount   int  `json:"user_count" db:"-"`
}

// NewOrganization is what an operator gives to create an organization.
type NewOrganization struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DeviceLimit *int   `json:"device_limit"`
}

// NewAdmin is what an operator gives to create an administrator of an organization.
type NewAdmin struct {
	Username string `json:"username"`
	// TwoFASendTo is required when the server sends 2FA tokens.
	TwoFASendTo string `json:"two_fa_send_to,omitempty"`
}

// Admin is a created administrator. Password is generated, shown only once, and must be changed at the first login.
type Admin struct {
	Organization    string `json:"organization"`
	Username        string `json:"username"`
	Password        string `json:"password"`
	PasswordExpired bool   `json:"password_expired"`
}

// Actor is who does an operator action, for the audit log.
type Actor struct {
	Username string
	RemoteIP string
}

// Service is the storage of the operator API. Every change is written to the audit log of SystemTenant.
type Service interface {
	// List returns every organization (tenants whose id doesn't start with "_"), oldest first.
	List(ctx context.Context) ([]Organization, error)
	// Get returns one organization or ErrNotFound.
	Get(ctx context.Context, id string) (*Organization, error)
	// Create adds an active organization with the built-in library entries.
	Create(ctx context.Context, by Actor, o NewOrganization) (*Organization, error)
	// SetStatus suspends (StatusSuspended) or reactivates (StatusActive) an organization. Users of a suspended
	// organization can't log in, and their sessions and API tokens stop working.
	SetStatus(ctx context.Context, by Actor, id, status string) (*Organization, error)
	// SetDeviceLimit sets the most devices the organization may have; nil is unlimited.
	SetDeviceLimit(ctx context.Context, by Actor, id string, limit *int) (*Organization, error)
	// AddAdmin creates a user in the organization's Administrators group with a generated password that must be
	// changed at the first login. id may be SystemTenant, which creates another operator.
	AddAdmin(ctx context.Context, by Actor, id string, a NewAdmin) (*Admin, error)
}
