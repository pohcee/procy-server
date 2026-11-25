package chconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stripeBilling() BillingConfig {
	return BillingConfig{Enabled: true, Provider: BillingProviderStripe, BillingProviderConfig: BillingProviderConfig{
		StripeSecretKeyFile: "/run/secrets/stripe-key", StripeWebhookSecretFile: "/run/secrets/stripe-webhook",
		ReturnURL: "https://procy.test/billing"}}
}

func TestBillingProviderNeedsPostgres(t *testing.T) {
	err := validate(&Config{Server: defaultValidMinServerConfig, Billing: stripeBilling()})
	assert.ErrorContains(t, err, `[billing] needs [storage] backend = "postgres"`)
	b := stripeBilling()
	b.Enabled = false
	err = validate(&Config{Server: defaultValidMinServerConfig, Billing: b})
	assert.ErrorContains(t, err, "enabled is false")
}

func TestBillingProviderStripe(t *testing.T) {
	c := postgresConfig()
	c.Billing = stripeBilling()
	require.NoError(t, validate(c))
	assert.Equal(t, DefaultBillingGraceDays, c.Billing.GraceDays)
	assert.Equal(t, DefaultBillingReadOnlyDays, c.Billing.ReadOnlyDays)
	assert.True(t, c.Billing.AutomaticTax(), "Stripe Tax is on by default")

	for name, change := range map[string]func(*BillingConfig){
		"no key file":       func(b *BillingConfig) { b.StripeSecretKeyFile = "" },
		"no webhook secret": func(b *BillingConfig) { b.StripeWebhookSecretFile = "" },
		"no return url":     func(b *BillingConfig) { b.ReturnURL = "" },
		"relative return":   func(b *BillingConfig) { b.ReturnURL = "/billing" },
		"unknown provider":  func(b *BillingConfig) { b.Provider = "paypal" },
		"grace too long":    func(b *BillingConfig) { b.GraceDays = 365 },
		"keys without stripe": func(b *BillingConfig) {
			b.Provider = "none"
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := postgresConfig()
			c.Billing = stripeBilling()
			change(&c.Billing)
			assert.Error(t, validate(c))
		})
	}
	c = postgresConfig()
	c.Billing = BillingConfig{Enabled: true, Provider: "none"}
	assert.NoError(t, validate(c))
}
