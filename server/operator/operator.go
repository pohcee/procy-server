// Package operator is the platform operator API of a server on Postgres: platform operators (the administrators of
// the reserved system tenant) list, create, suspend and reactivate organizations (tenants), set their device limits
// and create their first administrators. The storage lives in pgserver; standalone servers have no operator API.
package operator

import (
	"context"
	"errors"
	"time"

	"github.com/pohcee/procy-server/share/tenant"
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
	// ErrNoInvitation: the organization has no pending invitation of the login.
	ErrNoInvitation = errors.New("no pending invitation of that login")
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
	// HasPassword: the account has a password (an OIDC-only login has none).
	HasPassword bool `json:"has_password"`
	// OIDCLinked: an OIDC identity (Google) is pinned to the account.
	OIDCLinked bool `json:"oidc_linked"`
	// TwoFA: the account has a TOTP secret, or the server sends 2FA codes to every login.
	TwoFA bool `json:"two_fa"`
	// LastLoginAt is the member's last full sign-in to the organization; nil: never since it was recorded.
	LastLoginAt *time.Time `json:"last_login_at"`
}

// Activity is what an organization did lately, as Service.Organizations reads it for every organization at once.
type Activity struct {
	// DevicesOnline: devices with a live session, or a client record that is connected.
	DevicesOnline int
	// LastDeviceAt is the newest device connect or disconnect (now while one is connected without a live session).
	LastDeviceAt *time.Time
	// LastLoginAt is the newest sign-in of a member.
	LastLoginAt        *time.Time
	InvitationsPending int
	// FromSignup: a completed signup created the organization.
	FromSignup bool
}

// OrganizationDetail is an organization with its activity.
type OrganizationDetail struct {
	Organization
	Activity Activity
}

// DayUsage is the transfer of every organization together on a UTC day, and the devices that had a session then.
type DayUsage struct {
	Day time.Time
	// In is the transfer to the devices (tunnel_bytes_to_device + upload_bytes_to_devices), Out from them
	// (tunnel_bytes_to_user + api_bytes_out).
	In, Out int64
	Devices int64
}

// Device is a device (client) of an organization as operators see it: metadata only, from the stored document
// (name, version, OS) and the session registry. No addresses, tags, labels or attributes.
type Device struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Online   bool    `json:"online"`
	NodeID   *string `json:"node_id"`
	Version  string  `json:"version"`
	OS       string  `json:"os"`
	OSFamily string  `json:"os_family"`
	// ConnectedAt is the start of the live session.
	ConnectedAt *time.Time `json:"connected_at"`
	// LastSeenAt is now while online, else when it disconnected.
	LastSeenAt *time.Time `json:"last_seen_at"`
	// FirstSeenAt is when a device process first reported it (nil for records of nodes with role "all").
	FirstSeenAt *time.Time `json:"first_seen_at"`
}

// Invitation is an organization's pending invitation of an existing account.
type Invitation struct {
	Organization     string    `json:"organization"`
	OrganizationName string    `json:"organization_name"`
	Username         string    `json:"username"`
	Groups           []string  `json:"groups"`
	InvitedBy        string    `json:"invited_by"`
	CreatedAt        time.Time `json:"created_at"`
}

// Activity log sources.
const (
	SourceOrganization = "organization"
	SourceOperator     = "operator"
)

// ActivityKind selects audit log entries.
type ActivityKind string

// The activity kinds.
const (
	ActivityAll    ActivityKind = "all"
	ActivitySignIn ActivityKind = "sign_in"
	ActivityAdmin  ActivityKind = "admin"
)

// ActivityQuery selects a page of an organization's activity: at most Limit entries older than Before (nil: the
// newest).
type ActivityQuery struct {
	Kind   ActivityKind
	Limit  int
	Before *ActivityCursor
}

// ActivityCursor is the position of an entry in the activity, newest first.
type ActivityCursor struct {
	Timestamp time.Time
	ID        int64
}

// ActivityEntry is the metadata of an audit log entry: never its request or response.
type ActivityEntry struct {
	ID          int64     `json:"id" db:"id"`
	Timestamp   time.Time `json:"timestamp" db:"timestamp"`
	Source      string    `json:"source" db:"-"`
	Application string    `json:"application" db:"application"`
	Action      string    `json:"action" db:"action"`
	Username    string    `json:"username" db:"username"`
	RemoteIP    string    `json:"remote_ip" db:"remote_ip"`
	AffectedID  string    `json:"affected_id" db:"affected_id"`
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

	// Organizations returns every organization as List does, with its activity, in a fixed number of statements.
	Organizations(ctx context.Context) ([]OrganizationDetail, error)
	// UsageDays returns the transfer and devices of every organization together per UTC day in [from, to); days
	// without usage are left out.
	UsageDays(ctx context.Context, from, to time.Time) ([]DayUsage, error)
	// AwaitingSignups returns since when each signup awaiting an operator's approval waits.
	AwaitingSignups(ctx context.Context) ([]time.Time, error)
	// Devices returns the devices of the organization id as its device limit counts them.
	Devices(ctx context.Context, id string) ([]Device, error)
	// Invitations returns the pending invitations of the organization id (every organization when id is ""),
	// newest first.
	Invitations(ctx context.Context, id string) ([]Invitation, error)
	// ResendInvitation renews the invitation of username to id (created now, by the operator): it moves to the top
	// of the invitee's list. ErrNoInvitation.
	ResendInvitation(ctx context.Context, by Actor, id, username string) (*Invitation, error)
	// RevokeInvitation withdraws the invitation of username to id. ErrNoInvitation.
	RevokeInvitation(ctx context.Context, by Actor, id, username string) error
	// Activity returns a page of the organization's audit log, metadata only, newest first: its own entries and
	// the operators' entries about it.
	Activity(ctx context.Context, id string, q ActivityQuery) ([]ActivityEntry, error)

	// Nodes returns every node of the cluster registry with its stats, today being the UTC day before to and the
	// period the UTC days [from, to), and the live sessions whose node is not in the registry.
	Nodes(ctx context.Context, from, to time.Time) ([]Node, int64, error)
	// Node returns the node id with its stats as Nodes does, its days in [from, to) that have transfer or
	// connection events, the organizations with live sessions on it and its newest NodeEventsLimit connection
	// events. ErrNodeNotFound.
	Node(ctx context.Context, id string, from, to time.Time) (*NodeDetail, error)
	// NodeDevices returns a page of the devices with a live session on node id, by organization and name, and how
	// many there are. ErrNodeNotFound.
	NodeDevices(ctx context.Context, id string, limit, offset int) ([]NodeDevice, int, error)
}

// AuditApplicationOrganizationInvitation is an operator's change of an organization's invitation.
const AuditApplicationOrganizationInvitation = "operator.organization.invitation"

// AuditActionResend is the renewal of an invitation.
const AuditActionResend = "resend"

// SignInApplications are the audit log applications of sign-ins (ActivitySignIn).
var SignInApplications = []string{"auth.login", "auth.oidc"}

// DeviceWorkApplications are the audit log applications of work on devices, left out of ActivityAdmin with the
// sign-ins; DeviceWorkPrefixes are the prefixes of more (library.*).
var (
	DeviceWorkApplications = []string{"client.command", "client.script", "client.tunnel", "schedule", "uploads"}
	DeviceWorkPrefixes     = []string{"library."}
)
