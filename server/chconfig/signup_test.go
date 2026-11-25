package chconfig

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSignupNeedsPostgres(t *testing.T) {
	err := validate(&Config{Server: defaultValidMinServerConfig, Signup: SignupConfig{Enabled: true, PublicURL: "https://x.example", Mail: "log"}})
	assert.ErrorContains(t, err, `[signup] needs [storage] backend = "postgres"`)
	assert.NoError(t, validate(&Config{Server: defaultValidMinServerConfig}), "absent: off")
}

func TestSignupDefaultsAndChecks(t *testing.T) {
	c := postgresConfig()
	c.Signup = SignupConfig{Enabled: true, PublicURL: "https://ui.example/", Mail: SignupMailLog}
	require.NoError(t, validate(c))
	s := c.Signup
	assert.Equal(t, "https://ui.example", s.PublicURL)
	assert.Equal(t, DefaultSignupTokenTTL, s.TokenTTL)
	assert.Equal(t, DefaultSignupDeletionHold, s.DeletionHold)
	assert.Equal(t, DefaultSignupMaxPerIPPerDay, s.MaxPerIPPerDay)
	assert.Equal(t, DefaultSignupMaxPerDomainPerDay, s.MaxPerDomainPerDay)
	assert.Equal(t, DefaultSignupMaxPerEmailPerDay, s.MaxPerEmailPerDay)
	assert.Equal(t, DefaultSignupTrialDeviceLimit, s.TrialDeviceLimit)
	assert.Equal(t, DefaultSignupTrialDays, s.TrialDaysOrDefault())

	zero := 0
	neg := -1
	for name, sc := range map[string]SignupConfig{
		"no public url":       {Enabled: true, Mail: SignupMailLog},
		"bad public url":      {Enabled: true, PublicURL: "procy.sh", Mail: SignupMailLog},
		"captcha no secret":   {Enabled: true, PublicURL: "https://x", Mail: SignupMailLog, Captcha: SignupCaptchaTurnstile},
		"unknown captcha":     {Enabled: true, PublicURL: "https://x", Mail: SignupMailLog, Captcha: "recaptcha", CaptchaSecretFile: "/s"},
		"smtp not configured": {Enabled: true, PublicURL: "https://x"},
		"unknown mail":        {Enabled: true, PublicURL: "https://x", Mail: "carrier-pigeon"},
		"short token ttl":     {Enabled: true, PublicURL: "https://x", Mail: SignupMailLog, TokenTTL: time.Second},
		"negative trial":      {Enabled: true, PublicURL: "https://x", Mail: SignupMailLog, TrialDays: &neg},
		"negative hold":       {Enabled: true, PublicURL: "https://x", Mail: SignupMailLog, DeletionHold: -time.Hour},
	} {
		cc := postgresConfig()
		cc.Signup = sc
		assert.Error(t, validate(cc), name)
	}

	c = postgresConfig()
	c.Signup = SignupConfig{Enabled: true, PublicURL: "https://x", Mail: SignupMailLog, TrialDays: &zero, MaxPerIPPerDay: -1,
		Captcha: SignupCaptchaHCaptcha, CaptchaSecretFile: "/run/secrets/hcaptcha"}
	require.NoError(t, validate(c))
	assert.Equal(t, 0, c.Signup.TrialDaysOrDefault(), "no trial")
	assert.Equal(t, -1, c.Signup.MaxPerIPPerDay, "negative: off")
}
