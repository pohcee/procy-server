package chconfig

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Signup defaults.
const (
	DefaultSignupTokenTTL           = 24 * time.Hour
	DefaultSignupDeletionHold       = 30 * 24 * time.Hour
	DefaultSignupTrialDays          = 14
	DefaultSignupTrialDeviceLimit   = 25
	DefaultSignupMaxPerIPPerDay     = 3
	DefaultSignupMaxPerDomainPerDay = 10
	DefaultSignupMaxPerEmailPerDay  = 3

	SignupCaptchaTurnstile = "turnstile"
	SignupCaptchaHCaptcha  = "hcaptcha"

	// SignupMailSMTP sends the signup emails through the [smtp] server; SignupMailLog only logs them (with the
	// links), for development.
	SignupMailSMTP = "smtp"
	SignupMailLog  = "log"
)

// SignupConfig is the [signup] section: self-service signup (a new organization and its first administrator from a
// verified email), organization export and deletion. Postgres only; absent (off) by default.
type SignupConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// PublicURL is the address of the web UI the links in the emails point to (https://procy.sh). Required.
	PublicURL string `mapstructure:"public_url"`
	// RequireApproval holds verified signups until an operator approves them.
	RequireApproval bool `mapstructure:"require_approval"`
	// DefaultPlan is the plan of a new organization when there is no trial (TrialDays = 0 or no TrialPlan).
	DefaultPlan string `mapstructure:"default_plan"`
	// TrialPlan is the plan a new organization tries for TrialDays (default 14; 0: no trial).
	TrialPlan string `mapstructure:"trial_plan"`
	TrialDays *int   `mapstructure:"trial_days"`
	// TrialDeviceLimit is the device limit of a new organization until a payment method is on file, used while
	// the plans ([billing]) don't give one (default 25).
	TrialDeviceLimit int `mapstructure:"trial_device_limit"`
	// Captcha is "turnstile", "hcaptcha" or "" (none). The secret is read from CaptchaSecretFile; the site key is
	// public and given to the UI.
	Captcha           string `mapstructure:"captcha"`
	CaptchaSecretFile string `mapstructure:"captcha_secret_file"`
	CaptchaSiteKey    string `mapstructure:"captcha_site_key"`
	// BlockedDomainsFile lists email domains refused (disposable mail), one per line; subdomains are refused too.
	BlockedDomainsFile string `mapstructure:"blocked_domains_file"`
	// AllowedDomainsFile, when set, allows only the email domains it lists (private clusters).
	AllowedDomainsFile string `mapstructure:"allowed_domains_file"`
	// CheckMX refuses email domains without an MX (or address) record.
	CheckMX bool `mapstructure:"check_mx"`
	// UnlimitedDomains are email domains the per-domain limit doesn't apply to: shared mail providers, where it
	// would let a few requests block everyone's signup (default DefaultSignupUnlimitedDomains; [] for none).
	UnlimitedDomains []string `mapstructure:"unlimited_domains"`
	// Limits per day (24 hours); 0 takes the default, a negative value turns the limit off.
	MaxPerIPPerDay     int `mapstructure:"max_per_ip_per_day"`
	MaxPerDomainPerDay int `mapstructure:"max_per_domain_per_day"`
	MaxPerEmailPerDay  int `mapstructure:"max_per_email_per_day"`
	// TokenTTL is how long a verification link is valid (default 24h).
	TokenTTL time.Duration `mapstructure:"token_ttl"`
	// DeletionHold is how long a deleted organization is kept, suspended, before its data is deleted (default 30
	// days).
	DeletionHold time.Duration `mapstructure:"deletion_hold"`
	// Mail is "smtp" (default) or "log".
	Mail string `mapstructure:"mail"`
}

// DefaultSignupUnlimitedDomains are large shared mail providers.
var DefaultSignupUnlimitedDomains = []string{
	"gmail.com", "googlemail.com", "outlook.com", "hotmail.com", "live.com", "msn.com", "yahoo.com", "icloud.com",
	"me.com", "aol.com", "proton.me", "protonmail.com", "gmx.com", "gmx.net", "gmx.de", "web.de", "mail.com",
	"yandex.com", "zoho.com", "fastmail.com",
}

// UnlimitedDomain reports whether the per-domain limit is off for domain.
func (s SignupConfig) UnlimitedDomain(domain string) bool {
	list := s.UnlimitedDomains
	if list == nil {
		list = DefaultSignupUnlimitedDomains
	}
	for _, d := range list {
		if strings.EqualFold(d, domain) {
			return true
		}
	}
	return false
}

// TrialDaysOrDefault is TrialDays, or DefaultSignupTrialDays when it is not set.
func (s SignupConfig) TrialDaysOrDefault() int {
	if s.TrialDays == nil {
		return DefaultSignupTrialDays
	}
	return *s.TrialDays
}

func (c *Config) parseAndValidateSignup() error {
	s := &c.Signup
	if !s.Enabled {
		return nil
	}
	if !c.Storage.IsPostgres() {
		return errors.New(`[signup] needs [storage] backend = "postgres"`)
	}
	u, err := url.Parse(s.PublicURL)
	if s.PublicURL == "" || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("[signup] public_url must be the http(s) address of the web UI (the links in the signup emails point to it)")
	}
	s.PublicURL = strings.TrimRight(s.PublicURL, "/")
	switch s.Captcha {
	case "":
	case SignupCaptchaTurnstile, SignupCaptchaHCaptcha:
		if s.CaptchaSecretFile == "" {
			return fmt.Errorf("[signup] captcha = %q needs captcha_secret_file", s.Captcha)
		}
	default:
		return fmt.Errorf("[signup] captcha must be %q, %q or empty, it is %q", SignupCaptchaTurnstile, SignupCaptchaHCaptcha, s.Captcha)
	}
	switch s.Mail {
	case "":
		s.Mail = SignupMailSMTP
		fallthrough
	case SignupMailSMTP:
		if err := c.SMTP.Validate(); err != nil {
			return fmt.Errorf(`[signup] sends its emails through [smtp] (or set mail = "log" for development): %w`, err)
		}
	case SignupMailLog:
	default:
		return fmt.Errorf("[signup] mail must be %q or %q, it is %q", SignupMailSMTP, SignupMailLog, s.Mail)
	}
	if s.TrialDays != nil && *s.TrialDays < 0 {
		return errors.New("[signup] trial_days must not be negative")
	}
	if s.TrialDeviceLimit < 0 {
		return errors.New("[signup] trial_device_limit must not be negative")
	}
	if s.TrialDeviceLimit == 0 {
		s.TrialDeviceLimit = DefaultSignupTrialDeviceLimit
	}
	for _, l := range []struct {
		v   *int
		def int
	}{{&s.MaxPerIPPerDay, DefaultSignupMaxPerIPPerDay}, {&s.MaxPerDomainPerDay, DefaultSignupMaxPerDomainPerDay}, {&s.MaxPerEmailPerDay, DefaultSignupMaxPerEmailPerDay}} {
		if *l.v == 0 {
			*l.v = l.def
		}
	}
	switch {
	case s.TokenTTL == 0:
		s.TokenTTL = DefaultSignupTokenTTL
	case s.TokenTTL < 5*time.Minute || s.TokenTTL > 7*24*time.Hour:
		return fmt.Errorf("[signup] token_ttl must be between 5m and 168h, it is %s", s.TokenTTL)
	}
	switch {
	case s.DeletionHold == 0:
		s.DeletionHold = DefaultSignupDeletionHold
	case s.DeletionHold < 0:
		return errors.New("[signup] deletion_hold must not be negative")
	}
	return nil
}
