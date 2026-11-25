package pgserver

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/notifications/channels/rmailer"
	"github.com/openrport/openrport/server/operator"
	"github.com/openrport/openrport/server/signup"
	"github.com/openrport/openrport/server/storage/postgres"
	pgsignup "github.com/openrport/openrport/server/storage/postgres/signup"
	pgusers "github.com/openrport/openrport/server/storage/postgres/users"
	"github.com/openrport/openrport/server/vault"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

// signupOverrides replace the mailer, the captcha and the MX lookup of the signup service (tests).
var signupOverrides struct {
	mailer   signup.Mailer
	captcha  signup.Captcha
	lookupMX func(ctx context.Context, domain string) error
	plans    signup.Plans
}

// accounts are the accounts of signup (migrations 1400 and 1550).
type accounts struct{ db *sqlx.DB }

func (a accounts) Account(ctx context.Context, login string) (signup.Account, error) {
	return accountStatus(ctx, a.db, login)
}

// accountStatus reads the account of login, whatever tenant it is a member of.
func accountStatus(ctx context.Context, db *sqlx.DB, login string) (signup.Account, error) {
	var r struct {
		Found           bool `db:"account_exists"`
		Verified        bool `db:"verified"`
		PasswordExpired bool `db:"password_expired"`
	}
	if err := db.GetContext(ctx, &r, "SELECT account_exists, verified, password_expired FROM rport_account_status($1)", login); err != nil {
		return signup.Account{}, grantHint(fmt.Errorf("account of %q: %w", login, err))
	}
	return signup.Account{Exists: r.Found, Verified: r.Verified, PasswordExpired: r.PasswordExpired}, nil
}

func (a accounts) SetPasswordHash(ctx context.Context, org, login, hash string) error {
	return pgusers.New(a.db).Update(tenant.WithID(ctx, tenant.ID(org)), &users.User{Password: hash, PasswordExpired: users.PasswordExpired(false)}, login)
}

func (a accounts) MarkVerified(ctx context.Context, login string) error {
	return grantHint(postgres.InTenantID(ctx, a.db, operator.SystemTenant, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, "SELECT rport_account_verify_email($1)", login)
		return err
	}))
}

func (a accounts) TakeOver(ctx context.Context, login, hash string) (ok bool, err error) {
	err = postgres.InTenantID(ctx, a.db, operator.SystemTenant, func(tx *sqlx.Tx) error {
		return tx.GetContext(ctx, &ok, "SELECT rport_account_take_over($1, $2)", login, hash)
	})
	return ok, grantHint(err)
}

func (a accounts) Join(ctx context.Context, org, login string) error {
	return pgusers.New(a.db).AcceptInvitation(tenant.WithID(ctx, tenant.ID(org)), login)
}

// grantHint names the fix for an application role granted before migration 1550.
func grantHint(err error) error {
	if isCode(err, "42501") {
		return fmt.Errorf("%w (give the application role the signup privileges: run \"rportd migrate --grant-to ROLE\" as the schema owner)", err)
	}
	return err
}

// NewSignup returns the signup service of [signup] on db: organizations are created through op, emails go through
// [smtp] (or the log with mail = "log"), and the vault keys of deleted organizations are shredded when keys is the
// managed provider.
func NewSignup(cfg *chconfig.Config, db *sqlx.DB, op operator.Service, keys vault.KeyProvider) (*signup.Service, error) {
	sc := cfg.Signup
	log := logger.NewLogger("signup", cfg.Logging.LogOutput, cfg.Logging.LogLevel)
	domains, err := signup.LoadDomains(sc.BlockedDomainsFile, sc.AllowedDomainsFile)
	if err != nil {
		return nil, err
	}
	if sc.CheckMX {
		domains.LookupMX = signup.LookupMX
	}
	captcha, err := signup.NewCaptcha(sc)
	if err != nil {
		return nil, err
	}
	var mailer signup.Mailer
	switch sc.Mail {
	case chconfig.SignupMailLog:
		log.Infof(`[signup] mail = "log": signup emails are not sent; they are logged with their links (development only)`)
		mailer = signup.LogMailer{Log: log}
	default:
		smtp, err := rmailer.ConfigFromSMTPConfig(cfg.SMTP)
		if err != nil {
			return nil, fmt.Errorf("signup: [smtp]: %w", err)
		}
		mailer = signup.SMTPMailer{M: rmailer.NewRMailer(smtp, log.Fork("smtp"))}
	}
	var plans signup.Plans = signup.StandInPlans{Orgs: op, Config: sc}
	if o := signupOverrides; o.mailer != nil || o.captcha != nil || o.lookupMX != nil || o.plans != nil {
		if o.mailer != nil {
			mailer = o.mailer
		}
		if o.captcha != nil {
			captcha = o.captcha
		}
		if o.lookupMX != nil {
			domains.LookupMX = o.lookupMX
		}
		if o.plans != nil {
			plans = o.plans
		}
	}
	var shredder signup.Shredder
	if s, ok := keys.(signup.Shredder); ok {
		shredder = s
	}
	return signup.New(signup.Options{
		Config:    sc,
		Passwords: signup.PasswordRules{MinLength: cfg.API.PasswordMinLength, MinScore: cfg.API.PasswordZxcvbnMinscore},
		Store:     pgsignup.Store{DB: db},
		Orgs:      op,
		Accounts:  accounts{db: db},
		Plans:     plans,
		Mailer:    mailer,
		Captcha:   captcha,
		Domains:   domains,
		Shredder:  shredder,
		Log:       log,
	}), nil
}
