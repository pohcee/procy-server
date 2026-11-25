// Package membership is the API's view of logins that belong to several organizations (tenants): one account
// (login, password, TOTP secret) and a membership with its own permission groups in each organization.
//
// It exists only on Postgres nodes (ServerOpts.Memberships); a standalone server has one invisible organization and
// none of these routes.
package membership

import (
	"context"
	"errors"
)

var (
	// ErrAlreadyMember: the login is a member of the organization already.
	ErrAlreadyMember = errors.New("the login is a member of this organization already")
	// ErrInvalid: the invitation is not valid.
	ErrInvalid = errors.New("invalid invitation")
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
	// TwoFASendTo is where 2FA tokens of this membership go (required when the server sends 2FA tokens and the
	// login has no account yet).
	TwoFASendTo string `json:"two_fa_send_to,omitempty"`
}

// Invited is the result of an invitation.
type Invited struct {
	Organization string `json:"organization"`
	Username     string `json:"username"`
	// NewAccount is true when the login had no account: it was created with Password, shown only once, which must
	// be changed at the first login. An existing account became a member with its own password.
	NewAccount      bool   `json:"new_account"`
	Password        string `json:"password,omitempty"`
	PasswordExpired bool   `json:"password_expired"`
}

// Service is the storage behind the membership routes.
type Service interface {
	// Organizations returns the active organizations the login is a member of, by name.
	Organizations(ctx context.Context, username string) ([]Organization, error)
	// Remember makes the organization of ctx the one the next login of username that names none goes to.
	Remember(ctx context.Context, username string) error
	// MemberElsewhere reports whether the login is a member of an organization other than the one of ctx (of any
	// status): its password, TOTP secret and name are then its own, not one organization's.
	MemberElsewhere(ctx context.Context, username string) (bool, error)
	// Invite adds a login to the organization of ctx: an existing account becomes a member at once, a new login
	// gets an account with a generated one-time password. ErrAlreadyMember when it is a member already.
	Invite(ctx context.Context, in Invitation) (*Invited, error)
}
