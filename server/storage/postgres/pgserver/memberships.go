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
}

var _ membership.Service = (*Memberships)(nil)

// NewMemberships returns the membership service on db. New accounts are validated with the password and 2FA
// settings of cfg's [api] section.
func NewMemberships(db *sqlx.DB, cfg *chconfig.Config) *Memberships {
	store := pgusers.New(db)
	return &Memberships{
		store:          store,
		users:          users.NewAPIService(store, cfg.API.IsTwoFAOn(), cfg.API.PasswordMinLength, cfg.API.PasswordZxcvbnMinscore),
		twoFA:          cfg.API.IsTwoFAOn(),
		passwordLength: max(generatedPasswordLength, cfg.API.PasswordMinLength),
	}
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

// Remember implements membership.Service.
func (m *Memberships) Remember(ctx context.Context, username string) error {
	return m.store.Remember(ctx, username)
}

// MemberElsewhere implements membership.Service.
func (m *Memberships) MemberElsewhere(ctx context.Context, username string) (bool, error) {
	return m.store.MemberElsewhere(ctx, username)
}

// Invite implements membership.Service.
func (m *Memberships) Invite(ctx context.Context, in membership.Invitation) (*membership.Invited, error) {
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	return m.invite(tenant.WithID(ctx, tid), tid, in)
}

// invite adds in.Username to the tenant id (ctx is in it): an existing account joins, a new login gets an account
// with a generated password that must be changed at the first login.
func (m *Memberships) invite(ctx context.Context, id tenant.ID, in membership.Invitation) (*membership.Invited, error) {
	if in.Username == "" || strings.TrimSpace(in.Username) != in.Username {
		return nil, fmt.Errorf("%w: username is required and must not start or end with whitespace", membership.ErrInvalid)
	}
	if m.twoFA && in.TwoFASendTo == "" {
		return nil, fmt.Errorf("%w: two_fa_send_to is required", membership.ErrInvalid)
	}
	exists, err := m.store.AccountExists(ctx, in.Username)
	if err != nil {
		return nil, err
	}
	if exists {
		err := m.store.AddMember(ctx, &users.User{Username: in.Username, Groups: in.Groups, TwoFASendTo: in.TwoFASendTo})
		switch {
		case errors.Is(err, pgusers.ErrAlreadyMember):
			return nil, membership.ErrAlreadyMember
		case errors.Is(err, pgusers.ErrNoAccount):
			// the last membership went in the meantime: a new login after all
		case err != nil:
			return nil, err
		default:
			return &membership.Invited{Organization: string(id), Username: in.Username}, nil
		}
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
