package chconfig

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	chshare "github.com/openrport/openrport/share"
)

func TestParseAndValidatePairing(t *testing.T) {
	api := APIConfig{Address: "0.0.0.0:3000", Auth: "abc:def"}
	for _, tc := range []struct {
		Name          string
		API           APIConfig
		Pairing       PairingConfig
		ExpectedError string
		Check         func(t *testing.T, c *Config)
	}{
		{
			Name:    "disabled needs nothing",
			Pairing: PairingConfig{URL: "not a url"},
		}, {
			Name:          "enabled needs the API",
			Pairing:       PairingConfig{Enabled: true},
			ExpectedError: "pairing: the built-in pairing service needs the API to be enabled (api.address)",
		}, {
			Name:    "defaults",
			API:     api,
			Pairing: PairingConfig{Enabled: true, DownloadsURL: "https://rport.example.com/downloads/"},
			Check: func(t *testing.T, c *Config) {
				assert.Equal(t, time.Hour, c.Pairing.CodeTTL)
				assert.Equal(t, "https://rport.example.com/downloads", c.Pairing.DownloadsURL)
				assert.Equal(t, "", c.PairingPublicURL())
			},
		}, {
			Name:    "public url falls back to api.base_url",
			API:     APIConfig{Address: "0.0.0.0:3000", Auth: "abc:def", BaseURL: "https://rport.example.com:443/"},
			Pairing: PairingConfig{Enabled: true, CodeTTL: 30 * time.Minute},
			Check: func(t *testing.T, c *Config) {
				assert.Equal(t, 30*time.Minute, c.Pairing.CodeTTL)
				assert.Equal(t, "https://rport.example.com:443", c.PairingPublicURL())
			},
		}, {
			Name:    "pairing.url wins",
			API:     APIConfig{Address: "0.0.0.0:3000", Auth: "abc:def", BaseURL: "https://internal.example.com"},
			Pairing: PairingConfig{Enabled: true, URL: "https://rport.example.com/"},
			Check: func(t *testing.T, c *Config) {
				assert.Equal(t, "https://rport.example.com", c.PairingPublicURL())
			},
		}, {
			Name:          "invalid url",
			API:           api,
			Pairing:       PairingConfig{Enabled: true, URL: "rport.example.com"},
			ExpectedError: `pairing: invalid url rport.example.com: parse "rport.example.com": invalid URI for request`,
		}, {
			Name:          "invalid downloads url",
			API:           api,
			Pairing:       PairingConfig{Enabled: true, DownloadsURL: "ftp://x.example.com"},
			ExpectedError: "pairing: invalid url ftp://x.example.com: schema must be http or https",
		}, {
			Name:          "negative ttl",
			API:           api,
			Pairing:       PairingConfig{Enabled: true, CodeTTL: -time.Minute},
			ExpectedError: "pairing: code_ttl must be a positive duration, e.g. 1h",
		},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			c := Config{Server: defaultValidMinServerConfig, API: tc.API, Pairing: tc.Pairing}
			err := c.ParseAndValidate(&Mlog)
			if tc.ExpectedError != "" {
				require.EqualError(t, err, tc.ExpectedError)
				return
			}
			require.NoError(t, err)
			if tc.Check != nil {
				tc.Check(t, &c)
			}
		})
	}
}

func TestLoadingPairingConf(t *testing.T) {
	v := viper.New()
	v.SetConfigType("toml")
	cfg := &Config{}
	err := chshare.DecodeViperConfig(v, cfg, strings.NewReader(`
[pairing]
  enabled = true
  url = "https://rport.example.com"
  downloads_url = "https://rport.example.com/downloads"
  code_ttl = "30m"
  allow_code_reuse = true
`))
	require.NoError(t, err)
	assert.Equal(t, PairingConfig{
		Enabled:        true,
		URL:            "https://rport.example.com",
		DownloadsURL:   "https://rport.example.com/downloads",
		CodeTTL:        30 * time.Minute,
		AllowCodeReuse: true,
	}, cfg.Pairing)
}
