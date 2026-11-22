package chconfig

import (
	"errors"
	"strings"
	"time"

	"github.com/openrport/openrport/share/logger"
)

// DefaultPairingCodeTTL is how long a pairing code of the built-in pairing service stays valid.
const DefaultPairingCodeTTL = time.Hour

// PairingConfig configures the built-in pairing service ([pairing] section).
type PairingConfig struct {
	// Enabled serves the pairing endpoints under <url>/pairing and reports that URL as
	// the server's pairing_url, instead of [server] pairing_url.
	Enabled bool `mapstructure:"enabled"`
	// URL is rportd's public base URL used in install one-liners and scripts.
	// Empty = api.base_url, or derived from the request when that is empty too.
	URL string `mapstructure:"url"`
	// DownloadsURL is the base URL serving the client packages
	// (rport/<release>/rport_linux_<arch>.tar.gz, rport_windows_x86_64.zip, SHA256SUMS, VERSION).
	DownloadsURL string `mapstructure:"downloads_url"`
	// CodeTTL is how long a pairing code / install link stays valid.
	CodeTTL time.Duration `mapstructure:"code_ttl"`
	// AllowCodeReuse lets a code be fetched more than once until it expires.
	// By default the first successful fetch invalidates it.
	AllowCodeReuse bool `mapstructure:"allow_code_reuse"`
}

// PairingPublicURL returns the configured public base URL ("" = derive it from the request).
func (c *Config) PairingPublicURL() string {
	if c.Pairing.URL != "" {
		return c.Pairing.URL
	}
	return strings.TrimRight(c.API.BaseURL, "/")
}

func (c *Config) parseAndValidatePairing(mLog *logger.MemLogger) error {
	p := &c.Pairing
	if !p.Enabled {
		return nil
	}
	if c.API.Address == "" {
		return errors.New("the built-in pairing service needs the API to be enabled (api.address)")
	}
	p.URL = strings.TrimRight(strings.TrimSpace(p.URL), "/")
	if p.URL != "" {
		if err := validateHTTPorHTTPSURL(p.URL); err != nil {
			return err
		}
	}
	p.DownloadsURL = strings.TrimRight(strings.TrimSpace(p.DownloadsURL), "/")
	if p.DownloadsURL != "" {
		if err := validateHTTPorHTTPSURL(p.DownloadsURL); err != nil {
			return err
		}
	} else {
		mLog.Errorf("pairing: no downloads_url set, the generated install and update scripts will refuse to run")
	}
	if p.CodeTTL < 0 {
		return errors.New("code_ttl must be a positive duration, e.g. 1h")
	}
	if p.CodeTTL == 0 {
		p.CodeTTL = DefaultPairingCodeTTL
	}
	if c.PairingPublicURL() == "" {
		mLog.Infof("pairing: neither pairing.url nor api.base_url set, install links use the host of each request")
	}
	return nil
}
