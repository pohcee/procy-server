package chconfig

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// Defaults of [oidc].
const (
	DefaultOIDCIssuer      = "https://accounts.google.com"
	DefaultOIDCButtonLabel = "Sign in with Google"
	// OIDCCallbackPath is the path of the callback below the API's address; redirect_url must end with it.
	OIDCCallbackPath = "/api/v1/oidc/callback"
	// OIDCUIPath is the page of the web UI a finished sign-in returns to (below ui_url).
	OIDCUIPath = "/auth/oidc"
)

// OIDCConfig is [oidc]: sign-in through an OpenID Connect provider (Google). The API process uses it.
type OIDCConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// Issuer is the provider's issuer URL; its discovery document is at Issuer/.well-known/openid-configuration.
	Issuer   string `mapstructure:"issuer"`
	ClientID string `mapstructure:"client_id"`
	// ClientSecretFile holds the client secret (the secret is never read from the config itself).
	ClientSecretFile string `mapstructure:"client_secret_file"`
	// RedirectURL is the callback registered with the provider: https://<api domain>/api/v1/oidc/callback.
	RedirectURL string `mapstructure:"redirect_url"`
	// UIURL is the address of the web UI a sign-in returns to (UIURL/auth/oidc); default the scheme and host of
	// RedirectURL.
	UIURL string `mapstructure:"ui_url"`
	// AllowedDomains, when set, allows only these domains: the Google Workspace domain (hd claim) of the account,
	// else the domain of its email.
	AllowedDomains []string `mapstructure:"allowed_domains"`
	ButtonLabel    string   `mapstructure:"button_label"`

	// ClientSecret is read from ClientSecretFile by the validation.
	ClientSecret string `mapstructure:"-"`
}

// DomainAllowed reports whether a verified account of email (Workspace domain hd, "" for none) may sign in.
func (o OIDCConfig) DomainAllowed(email, hd string) bool {
	if len(o.AllowedDomains) == 0 {
		return true
	}
	domain := hd
	if domain == "" {
		at := strings.LastIndex(email, "@")
		if at < 0 {
			return false
		}
		domain = email[at+1:]
	}
	for _, d := range o.AllowedDomains {
		if strings.EqualFold(strings.TrimSpace(d), domain) {
			return true
		}
	}
	return false
}

// UIReturnURL is the page of the web UI a sign-in returns to.
func (o OIDCConfig) UIReturnURL() string {
	return strings.TrimRight(o.UIURL, "/") + OIDCUIPath
}

// secureURL accepts https URLs, and http ones only for a loopback host (tests and local development).
func secureURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, false
	}
	switch u.Scheme {
	case "https":
		return u, true
	case "http":
		host := u.Hostname()
		ip := net.ParseIP(host)
		return u, host == "localhost" || (ip != nil && ip.IsLoopback())
	}
	return nil, false
}

func (c *Config) parseAndValidateOIDC() error {
	o := &c.OIDC
	if !o.Enabled {
		return nil
	}
	if o.Issuer == "" {
		o.Issuer = DefaultOIDCIssuer
	}
	if _, ok := secureURL(o.Issuer); !ok {
		return fmt.Errorf("[oidc] issuer must be an https URL, it is %q", o.Issuer)
	}
	if o.ButtonLabel == "" {
		o.ButtonLabel = DefaultOIDCButtonLabel
	}
	if o.ClientID == "" {
		return errors.New("[oidc] client_id is required")
	}
	if o.ClientSecretFile == "" {
		return errors.New("[oidc] client_secret_file is required (the client secret is read only from a file)")
	}
	b, err := os.ReadFile(o.ClientSecretFile)
	if err != nil {
		return fmt.Errorf("[oidc] client_secret_file: %w", err)
	}
	o.ClientSecret = strings.TrimSpace(string(b))
	if o.ClientSecret == "" {
		return fmt.Errorf("[oidc] client_secret_file %q is empty", o.ClientSecretFile)
	}
	ru, ok := secureURL(o.RedirectURL)
	if !ok || !strings.HasSuffix(ru.Path, "/oidc/callback") || ru.RawQuery != "" || ru.Fragment != "" {
		return fmt.Errorf("[oidc] redirect_url must be the https address of the callback, https://<api domain>%s, it is %q",
			OIDCCallbackPath, o.RedirectURL)
	}
	if o.UIURL == "" {
		o.UIURL = ru.Scheme + "://" + ru.Host
	}
	if _, ok := secureURL(o.UIURL); !ok {
		return fmt.Errorf("[oidc] ui_url must be the https address of the web UI, it is %q", o.UIURL)
	}
	o.UIURL = strings.TrimRight(o.UIURL, "/")
	for _, d := range o.AllowedDomains {
		if strings.TrimSpace(d) == "" || strings.ContainsAny(d, "@/ ") {
			return fmt.Errorf("[oidc] allowed_domains: %q is not a domain", d)
		}
	}
	return nil
}
