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
	AuditActionDelete                 = "delete"
	// AuditApplicationOrganizationSignIn is an operator's change of an organization's sign-in policy.
	AuditApplicationOrganizationSignIn = "operator.organization.signin"
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
	// ErrNotMember: the login is not a member of the organization.
	ErrNotMember = errors.New("not a member of the organization")
	// ErrLastAdmin: removing the login would leave the organization without an administrator.
	ErrLastAdmin = errors.New("the last administrator of an organization can't be removed")
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
	// RequireOIDC: the organization's members sign in through OIDC only.
	RequireOIDC bool `json:"require_oidc" db:"require_oidc"`
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

// Admin is an added administrator. For a new login (NewAccount) Password is generated, shown only once, and must be
// changed at the first login. An existing account (a member of other organizations) gets a pending invitation
// (Pending): it becomes an administrator, with its own password, when its owner accepts; Password is left out.
type Admin struct {
	Organization    string `json:"organization"`
	Username        string `json:"username"`
	NewAccount      bool   `json:"new_account"`
	Password        string `json:"password,omitempty"`
	PasswordExpired bool   `json:"password_expired"`
	Pending         bool   `json:"pending"`
	// NoPassword: the new login has no password; it signs in through OIDC only (operators, and organizations that
	// require OIDC, with [oidc] enabled).
	NoPassword bool `json:"no_password,omitempty"`
}

// Member is a member of an organization as operators see it.
type Member struct {
	Username string   `json:"username"`
	Groups   []string `json:"groups"`
	// Organizations is how many organizations the login is a member of, this one included.
	Organizations int `json:"organizations"`
	// SignIn is the member's sign-in method: both, password or oidc.
	SignIn string `json:"sign_in"`
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
	// AddAdmin adds a login to the organization's Administrators group: a new login gets an account with a
	// generated password that must be changed at the first login, an existing account (of other organizations) gets
	// an invitation its owner accepts. ErrExists when it is a member already. id may be SystemTenant, which
	// adds another operator.
	AddAdmin(ctx context.Context, by Actor, id string, a NewAdmin) (*Admin, error)
	// Members lists the members of the organization id with their groups, by login.
	Members(ctx context.Context, id string) ([]Member, error)
	// SetRequireOIDC sets whether the organization's members sign in through OIDC only.
	SetRequireOIDC(ctx context.Context, by Actor, id string, require bool) (*Organization, error)
	// RemoveAdmin removes the administrator username from the organization id (SystemTenant: an operator): its
	// membership, sessions and API tokens there. ErrNotMember, ErrInvalid (not an administrator); ErrLastAdmin
	// unless force.
	RemoveAdmin(ctx context.Context, by Actor, id, username string, force bool) error
}
