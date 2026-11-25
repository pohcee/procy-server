// Package signup is self-service signup on a server on Postgres ([signup] enabled): a visitor gives an email, an
// organization name and a password, confirms the email through a single-use link, and gets a new organization with
// themself as its first administrator, on the trial plan. Organization administrators can export their
// organization's configuration and delete it (after a hold); platform operators list and approve signups and start,
// cancel or expedite deletions. Every action is written to the audit log of the system tenant.
//
// The storage is the Store interface (Postgres: storage/postgres/signup); organizations are created and suspended
// through operator.Service; plans and trials come from billing through Plans (StandInPlans until the plans exist).
// Standalone servers have none of it.
package signup

import (
	"context"
	"errors"
	"time"

	"github.com/openrport/openrport/server/operator"
	"github.com/openrport/openrport/share/tenant"
)

// Signup statuses.
const (
	StatusPending          = "pending"
	StatusExisting         = "existing"
	StatusAwaitingApproval = "awaiting_approval"
	StatusCompleted        = "completed"
	StatusDenied           = "denied"
	StatusExpired          = "expired"
)

// Deletion states.
const (
	DeletionPending  = "pending"
	DeletionCanceled = "canceled"
	DeletionDone     = "done"
)

// Audit log applications and actions, written in the system tenant.
const (
	AuditApplicationSignup   = "signup"
	AuditApplicationDeletion = "organization.deletion"
	AuditActionRequest       = "request"
	AuditActionVerify        = "verify"
	AuditActionCreate        = "create"
	AuditActionApprove       = "approve"
	AuditActionDeny          = "deny"
	AuditActionCancel        = "cancel"
	AuditActionExpedite      = "expedite"
	AuditActionDelete        = "delete"
	AuditActionExport        = "export"
)

var (
	// ErrInvalid: the input is not valid; wrapped with the reason.
	ErrInvalid = errors.New("invalid input")
	// ErrCaptcha: the captcha was not solved.
	ErrCaptcha = errors.New("the captcha check failed")
	// ErrDomain: the email domain is not accepted.
	ErrDomain = errors.New("this email domain is not accepted for signup")
	// ErrRateLimited: too many signups from this address, domain or email today.
	ErrRateLimited = errors.New("too many signups; try again tomorrow")
	// ErrToken: the link is unknown, used or expired.
	ErrToken = errors.New("the link is not valid: it was used already or it expired")
	// ErrNotFound: no such signup or deletion.
	ErrNotFound = errors.New("not found")
	// ErrConflict: the signup or the organization is not in a state that allows this.
	ErrConflict = errors.New("conflict")
	// ErrMail: the email could not be sent.
	ErrMail = errors.New("the email could not be sent")
)

// Signup is one accepted signup request.
type Signup struct {
	ID             string     `json:"id" db:"id"`
	Email          string     `json:"email" db:"email"`
	Domain         string     `json:"domain" db:"domain"`
	RemoteIP       string     `json:"remote_ip" db:"remote_ip"`
	OrgName        string     `json:"organization_name" db:"org_name"`
	PasswordHash   string     `json:"-" db:"password_hash"`
	TokenHash      *string    `json:"-" db:"token_hash"`
	Status         string     `json:"status" db:"status"`
	CreatedAt      time.Time  `json:"created_at" db:"created_at"`
	ExpiresAt      time.Time  `json:"expires_at" db:"expires_at"`
	VerifiedAt     *time.Time `json:"verified_at" db:"verified_at"`
	DecidedAt      *time.Time `json:"decided_at" db:"decided_at"`
	DecidedBy      string     `json:"decided_by" db:"decided_by"`
	Reason         string     `json:"reason" db:"reason"`
	OrganizationID *string    `json:"organization_id" db:"organization_id"`
}

// Update changes a signup's status (and what goes with it). PasswordHash is cleared when ClearPassword.
type Update struct {
	Status         string
	VerifiedAt     *time.Time
	ExpiresAt      *time.Time
	DecidedAt      *time.Time
	DecidedBy      string
	Reason         string
	OrganizationID string
	ClearPassword  bool
}

// Counts are the signups since a time from one IP, email domain and email.
type Counts struct {
	IP, Domain, Email int
}

// Deletion is a request to delete an organization.
type Deletion struct {
	ID               int64      `json:"id" db:"id"`
	OrganizationID   string     `json:"organization_id" db:"organization_id"`
	OrganizationName string     `json:"organization_name" db:"organization_name"`
	State            string     `json:"state" db:"state"`
	RequestedAt      time.Time  `json:"requested_at" db:"requested_at"`
	RequestedBy      string     `json:"requested_by" db:"requested_by"`
	Reason           string     `json:"reason" db:"reason"`
	DeleteAfter      time.Time  `json:"delete_after" db:"delete_after"`
	CancelTokenHash  *string    `json:"-" db:"cancel_token_hash"`
	PreviousStatus   string     `json:"previous_status" db:"previous_status"`
	FinishedAt       *time.Time `json:"finished_at" db:"finished_at"`
	FinishedBy       string     `json:"finished_by" db:"finished_by"`
}

// AuditEntry is a signup or deletion action for the system tenant's audit log.
type AuditEntry struct {
	Time        time.Time
	Actor       operator.Actor
	Application string
	Action      string
	ID          string
	Request     any
}

// Store keeps signups and deletions (in the system tenant) and does what needs direct database access.
type Store interface {
	// CountSince counts the signups created at or after since from ip, in domain and for email.
	CountSince(ctx context.Context, since time.Time, ip, domain, email string) (Counts, error)
	Insert(ctx context.Context, s Signup) error
	Get(ctx context.Context, id string) (*Signup, error)
	// List returns the newest signups first; status "" is every status.
	List(ctx context.Context, status string, limit int) ([]Signup, error)
	// ClaimToken takes the pending, unexpired signup whose token hash is tokenHash and makes its token unusable
	// (single use), atomically. ErrToken when there is none.
	ClaimToken(ctx context.Context, tokenHash string, now time.Time) (*Signup, error)
	// RestoreToken gives a claimed signup its token back (its creation failed and may be retried).
	RestoreToken(ctx context.Context, id, tokenHash string) error
	SetStatus(ctx context.Context, id string, u Update) error
	// Expire marks pending and awaiting signups whose expiry passed as expired and clears their passwords.
	Expire(ctx context.Context, now time.Time) (int, error)
	// UserExists reports whether username is a user of any tenant.
	UserExists(ctx context.Context, username string) (bool, error)
	// AdminEmails returns the email addresses of the administrators of the organization.
	AdminEmails(ctx context.Context, org string) ([]string, error)

	// AddDeletion records a pending deletion; ErrConflict when one is pending for the organization already.
	AddDeletion(ctx context.Context, d Deletion) (*Deletion, error)
	PendingDeletion(ctx context.Context, org string) (*Deletion, error)
	DeletionByCancelToken(ctx context.Context, tokenHash string) (*Deletion, error)
	// FinishDeletion sets the state (canceled or done) of a pending deletion; ErrNotFound when none is pending.
	FinishDeletion(ctx context.Context, org, state, by string, at time.Time) (*Deletion, error)
	// SetDeleteAfter moves the date of a pending deletion; a zero time is now by the database clock.
	SetDeleteAfter(ctx context.Context, org string, at time.Time) error
	// DueDeletions are the pending deletions whose date is at or before now.
	DueDeletions(ctx context.Context, now time.Time) ([]Deletion, error)
	ListDeletions(ctx context.Context, state string, limit int) ([]Deletion, error)
	// DeleteOrganization deletes the tenant org of a due deletion and marks the deletion done, in one transaction.
	// deleted is false (nothing changed) when the organization is not suspended.
	DeleteOrganization(ctx context.Context, org, by string, at time.Time) (deleted bool, _ error)

	// Export returns the configuration of the organization, without secrets.
	Export(ctx context.Context, org string) (*Export, error)

	Audit(ctx context.Context, e AuditEntry) error
}

// Credentials sets the password of a user from a bcrypt hash.
type Credentials interface {
	SetPasswordHash(ctx context.Context, org, username, hash string) error
}

// Plans is what signup needs from billing (billing-plans stream): the plan and limits of a new organization, and
// closing its billing when it is deleted.
type Plans interface {
	// StartOrganization puts a new organization on the trial plan (or the default plan without a trial), applies
	// the plan's limits for an organization without a payment method, and says whether the user must go to
	// checkout before using it.
	StartOrganization(ctx context.Context, by operator.Actor, org string) (*PlanStart, error)
	// DeletionRequested is called when the organization's deletion is requested: cancel its subscription and
	// report the final usage.
	DeletionRequested(ctx context.Context, by operator.Actor, org string) error
	// BeforeDelete is called just before the organization's data is deleted: detach the billing records that are
	// kept for accounting.
	BeforeDelete(ctx context.Context, org string) error
}

// PlanStart is the plan a new organization starts on.
type PlanStart struct {
	Plan string `json:"plan"`
	// BillingStatus is "trial", or the status of the default plan ("paying" for a free plan)
	BillingStatus    string     `json:"billing_status"`
	TrialEndsAt      *time.Time `json:"trial_ends_at,omitempty"`
	CheckoutRequired bool       `json:"checkout_required"`
}

// Shredder destroys the vault key of a tenant (vaultkms.Provider). nil when the vault uses a passphrase: its values
// are deleted with the tenant.
type Shredder interface {
	Shred(ctx context.Context, tid tenant.ID) error
}

// Mailer sends a plain text email.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// Captcha checks the response of a captcha widget.
type Captcha interface {
	Verify(ctx context.Context, response, remoteIP string) error
}

// Export is the configuration of an organization, without secrets (no passwords, TOTP secrets, API tokens, client
// credentials or vault values).
type Export struct {
	Format       string         `json:"format"`
	Organization map[string]any `json:"organization"`
	ExportedAt   time.Time      `json:"exported_at"`
	Users        []any          `json:"users"`
	UserGroups   []any          `json:"user_groups"`
	Clients      []any          `json:"clients"`
	ClientGroups []any          `json:"client_groups"`
	Tunnels      []any          `json:"stored_tunnels"`
	Schedules    []any          `json:"schedules"`
	Library      []any          `json:"library"`
}

// ExportFormat names the export's format and version.
const ExportFormat = "procy-organization-export/1"
