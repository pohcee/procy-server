package pgserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/membership"
	pgusers "github.com/openrport/openrport/server/storage/postgres/users"
	"github.com/openrport/openrport/share/tenant"
)

// Memberships is the Postgres membership.Service: accounts shared by several organizations (migration 1400).
type Memberships struct {
	store *pgusers.Store
	// users creates new accounts with the [api] password and 2FA rules
	users *users.APIService
	twoFA bool
	// passwordLength is the length of generated passwords: at least [api] password_min_length
	passwordLength int
	// db, with [signup] on, checks that an existing account proved its email (signup) before it is invited into
	// or joins another organization; nil: no check (no way to prove it without signup's emails)
	db *sqlx.DB
}

var _ membership.Service = (*Memberships)(nil)

// NewMemberships returns the membership service on db. New accounts are validated with the password and 2FA
// settings of cfg's [api] section.
func NewMemberships(db *sqlx.DB, cfg *chconfig.Config) *Memberships {
	store := pgusers.New(db)
	m := &Memberships{
		store:          store,
		users:          users.NewAPIService(store, cfg.API.IsTwoFAOn(), cfg.API.PasswordMinLength, cfg.API.PasswordZxcvbnMinscore),
		twoFA:          cfg.API.IsTwoFAOn(),
		passwordLength: max(generatedPasswordLength, cfg.API.PasswordMinLength),
	}
	if cfg.Signup.Enabled {
		m.db = db
	}
	return m
}

// ErrUnverified: an existing account that didn't prove its email can't be invited into more organizations.
var ErrUnverified = fmt.Errorf("%w: the account has not confirmed its email address yet (POST /me/email-confirmation)", membership.ErrInvalid)

// checkVerified refuses an unverified account when [signup] is on: an administrator could otherwise create an
// account for someone else's address and use it in further organizations before its owner signs up.
func (m *Memberships) checkVerified(ctx context.Context, username string) error {
	if m.db == nil {
		return nil
	}
	a, err := accountStatus(ctx, m.db, username)
	if err != nil {
		return err
	}
	if a.Exists && !a.Verified {
		return ErrUnverified
	}
	return nil
}

// Organizations implements membership.Service.
func (m *Memberships) Organizations(ctx context.Context, username string) ([]membership.Organization, error) {
	orgs, err := m.store.Organizations(ctx, username)
	if err != nil {
		return nil, err
	}
	out := make([]membership.Organization, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, membership.Organization{ID: o.ID, Name: o.Name})
	}
	return out, nil
}

// MemberElsewhere implements membership.Service.
func (m *Memberships) MemberElsewhere(ctx context.Context, username string) (bool, error) {
	return m.store.MemberElsewhere(ctx, username)
}

// Invite implements membership.Service.
func (m *Memberships) Invite(ctx context.Context, in membership.Invitation, invitedBy string) (*membership.Invited, error) {
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	return m.invite(tenant.WithID(ctx, tid), tid, in, invitedBy)
}

// invite adds in.Username to the tenant id (ctx is in it): an existing account gets a pending invitation its owner
// accepts, a new login gets an account with a generated password that must be changed at the first login.
func (m *Memberships) invite(ctx context.Context, id tenant.ID, in membership.Invitation, invitedBy string) (*membership.Invited, error) {
	if in.Username == "" || strings.TrimSpace(in.Username) != in.Username {
		return nil, fmt.Errorf("%w: username is required and must not start or end with whitespace", membership.ErrInvalid)
	}
	exists, err := m.store.AccountExists(ctx, in.Username)
	if err != nil {
		return nil, err
	}
	if exists {
		if err := m.checkVerified(ctx, in.Username); err != nil {
			return nil, err
		}
		err := m.store.Invite(ctx, in.Username, in.Groups, invitedBy)
		switch {
		case errors.Is(err, pgusers.ErrAlreadyMember):
			return nil, membership.ErrAlreadyMember
		case errors.Is(err, pgusers.ErrNoAccount):
			// the last membership went in the meantime: a new login after all
		case err != nil:
			return nil, err
		default:
			return &membership.Invited{Organization: string(id), Username: in.Username, Pending: true}, nil
		}
	}
	if in.NoPassword {
		u := &users.User{Username: in.Username, Groups: in.Groups, TwoFASendTo: in.TwoFASendTo}
		if err := m.users.AddWithoutPassword(ctx, u); err != nil {
			return nil, err
		}
		return &membership.Invited{Organization: string(id), Username: in.Username, NewAccount: true}, nil
	}
	if m.twoFA && in.TwoFASendTo == "" {
		return nil, fmt.Errorf("%w: two_fa_send_to is required", membership.ErrInvalid)
	}
	password, err := generatePassword(m.passwordLength)
	if err != nil {
		return nil, err
	}
	u := &users.User{
		Username:        in.Username,
		Password:        password,
		PasswordExpired: users.PasswordExpired(true),
		Groups:          in.Groups,
		TwoFASendTo:     in.TwoFASendTo,
	}
	if err := m.users.Change(ctx, u, ""); err != nil {
		return nil, err
	}
	return &membership.Invited{Organization: string(id), Username: in.Username, NewAccount: true, Password: password, PasswordExpired: true}, nil
}

// PendingInvitations implements membership.Service.
func (m *Memberships) PendingInvitations(ctx context.Context) ([]membership.PendingInvitation, error) {
	invs, err := m.store.PendingInvitations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]membership.PendingInvitation, 0, len(invs))
	for _, i := range invs {
		out = append(out, membership.PendingInvitation{Username: i.Username, Groups: i.Groups, InvitedBy: i.InvitedBy, CreatedAt: i.CreatedAt})
	}
	return out, nil
}

// Withdraw implements membership.Service.
func (m *Memberships) Withdraw(ctx context.Context, username string) error {
	err := m.store.DeleteInvitation(ctx, username)
	if errors.Is(err, pgusers.ErrNoInvitation) {
		return membership.ErrNoInvitation
	}
	return err
}

// Invitations implements membership.Service.
func (m *Memberships) Invitations(ctx context.Context, username string) ([]membership.MyInvitation, error) {
	invs, err := m.store.Invitations(ctx, username)
	if err != nil {
		return nil, err
	}
	out := make([]membership.MyInvitation, 0, len(invs))
	for _, i := range invs {
		out = append(out, membership.MyInvitation{Organization: membership.Organization{ID: i.TenantID, Name: i.Name},
			InvitedBy: i.InvitedBy, CreatedAt: i.CreatedAt})
	}
	return out, nil
}

// Accept implements membership.Service.
func (m *Memberships) Accept(ctx context.Context, username string) error {
	if err := m.checkVerified(ctx, username); err != nil {
		return err
	}
	err := m.store.AcceptInvitation(ctx, username)
	switch {
	case errors.Is(err, pgusers.ErrNoInvitation), errors.Is(err, pgusers.ErrNoAccount):
		return membership.ErrNoInvitation
	case errors.Is(err, pgusers.ErrAlreadyMember):
		return membership.ErrAlreadyMember
	}
	return err
}
