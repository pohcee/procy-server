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

// credentials sets a user's password from the bcrypt hash a signup stored.
type credentials struct{ db *sqlx.DB }

func (c credentials) SetPasswordHash(ctx context.Context, org, username, hash string) error {
	return pgusers.New(c.db).Update(tenant.WithID(ctx, tenant.ID(org)), &users.User{Password: hash, PasswordExpired: users.PasswordExpired(false)}, username)
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
		Config:      sc,
		Passwords:   signup.PasswordRules{MinLength: cfg.API.PasswordMinLength, MinScore: cfg.API.PasswordZxcvbnMinscore},
		Store:       pgsignup.Store{DB: db},
		Orgs:        op,
		Credentials: credentials{db: db},
		Plans:       plans,
		Mailer:      mailer,
		Captcha:     captcha,
		Domains:     domains,
		Shredder:    shredder,
		Log:         log,
	}), nil
}
