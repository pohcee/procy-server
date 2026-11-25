package chconfig

import "errors"

// STAND-IN for the billing-plans stream's [billing] section (feat/billing-plans): delete this file when merging
// it, embed BillingProviderConfig in its BillingConfig with `mapstructure:",squash"` and call validateProvider
// from its validation.

// BillingConfig is the [billing] section. (stand-in)
type BillingConfig struct {
	Enabled               bool   `mapstructure:"enabled"`
	Provider              string `mapstructure:"provider"`
	PlansFile             string `mapstructure:"plans_file"`
	BillingProviderConfig `mapstructure:",squash"`
}

func (c *Config) parseAndValidateBilling() error {
	b := &c.Billing
	if !b.Enabled {
		if b.Provider != "" || b.StripeSecretKeyFile != "" || b.StripeWebhookSecretFile != "" {
			return errors.New("[billing] settings are given but enabled is false")
		}
		return nil
	}
	if !c.Storage.IsPostgres() {
		return errors.New(`[billing] needs [storage] backend = "postgres"`)
	}
	return b.validateProvider(b.Provider)
}
