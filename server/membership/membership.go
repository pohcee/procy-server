// Package membership is the API's view of logins that belong to several organizations (tenants): one account
// (login, password, TOTP secret) and a membership with its own permission groups in each organization.
//
// It exists only on Postgres nodes (ServerOpts.Memberships); a standalone server has one invisible organization and
// none of these routes.
package membership

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrAlreadyMember: the login is a member of the organization already.
	ErrAlreadyMember = errors.New("the login is a member of this organization already")
	// ErrInvalid: the invitation is not valid.
	ErrInvalid = errors.New("invalid invitation")
	// ErrNoInvitation: there is no pending invitation.
	ErrNoInvitation = errors.New("no pending invitation")
)

// Organization is an organization a login is a member of.
type Organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Invitation is what an organization's administrator gives to add a login to the organization.
type Invitation struct {
	// Username is the login, normally an email address.
	Username string   `json:"username"`
	Groups   []string `json:"groups"`
	// TwoFASendTo is where the 2FA tokens of a new login go (required when the server sends 2FA tokens). It is the
	// account's, so it is ignored for an existing account.
	TwoFASendTo string `json:"two_fa_send_to,omitempty"`
	// NoPassword creates a new login without a password (it signs in through OIDC only). Set by the server, not
	// by clients.
	NoPassword bool `json:"-"`
}

// Invited is the result of an invitation.
type Invited struct {
	Organization string `json:"organization"`
	Username     string `json:"username"`
	// NewAccount is true when the login had no account: it was created as a member with Password, shown only
	// once, which must be changed at the first login.
	NewAccount      bool   `json:"new_account"`
	Password        string `json:"password,omitempty"`
	PasswordExpired bool   `json:"password_expired"`
	// Pending is true for an existing account: it becomes a member when its owner accepts the invitation
	// (POST /me/invitations/{organization}).
	Pending bool `json:"pending"`
}

// MyInvitation is a pending invitation of the current login into an organization.
type MyInvitation struct {
	Organization
	InvitedBy string    `json:"invited_by"`
	CreatedAt time.Time `json:"created_at"`
}

// PendingInvitation is an organization's pending invitation of an existing account.
type PendingInvitation struct {
	Username  string    `json:"username"`
	Groups    []string  `json:"groups"`
	InvitedBy string    `json:"invited_by"`
	CreatedAt time.Time `json:"created_at"`
}

// Service is the storage behind the membership routes.
type Service interface {
	// Organizations returns the active organizations the login is a member of, by name.
	Organizations(ctx context.Context, username string) ([]Organization, error)
	// MemberElsewhere reports whether the login is a member of an organization other than the one of ctx (of any
	// status): its password, 2FA address, TOTP secret and name are then its own, not one organization's.
	MemberElsewhere(ctx context.Context, username string) (bool, error)
	// Invite adds a login to the organization of ctx, invited by invitedBy: a new login gets an account with a
	// generated one-time password and is a member at once; an existing account gets a pending invitation its owner
	// accepts. ErrAlreadyMember when it is a member already.
	Invite(ctx context.Context, in Invitation, invitedBy string) (*Invited, error)
	// PendingInvitations lists the organization of ctx's pending invitations.
	PendingInvitations(ctx context.Context) ([]PendingInvitation, error)
	// Withdraw removes the organization of ctx's pending invitation of username (also a decline by its owner).
	// ErrNoInvitation when there is none.
	Withdraw(ctx context.Context, username string) error
	// Invitations lists the pending invitations of the login username from active organizations.
	Invitations(ctx context.Context, username string) ([]MyInvitation, error)
	// Accept makes username a member of the organization of ctx with the groups of its pending invitation there.
	// Only for the login's own owner. ErrNoInvitation, ErrAlreadyMember.
	Accept(ctx context.Context, username string) error
}
