package chconfig

import (
	"errors"
	"fmt"
	"net/url"
)

// BillingProviderStripe is the [billing] provider of Stripe.
const BillingProviderStripe = "stripe"

func init() { BillingProviders[BillingProviderStripe] = true }

// Dunning defaults and bounds ([billing] grace_days, read_only_days).
const (
	DefaultBillingGraceDays    = 14
	DefaultBillingReadOnlyDays = 14
	maxBillingDunningDays      = 90
)

// BillingProviderConfig is the payment provider part of [billing] (embedded in BillingConfig). Secrets are read
// only from files (Secret Manager in the hosted deployment): never from the configuration file itself, the
// database or the UI.
type BillingProviderConfig struct {
	// StripeSecretKeyFile holds the Stripe secret (or restricted) API key.
	StripeSecretKeyFile string `mapstructure:"stripe_secret_key_file"`
	// StripeWebhookSecretFile holds the signing secret of the webhook endpoint (/api/v1/billing/webhook).
	StripeWebhookSecretFile string `mapstructure:"stripe_webhook_secret_file"`
	// StripeAutomaticTax turns on Stripe Tax in checkout (default true: US and Canada sales tax, decision 12).
	StripeAutomaticTax *bool `mapstructure:"stripe_automatic_tax"`
	// ReturnURL is the UI's billing page: checkout and the customer portal send the user back there.
	ReturnURL string `mapstructure:"return_url"`
	// GraceDays of full service after a failed payment, then ReadOnlyDays read-only before a suspension is
	// proposed to operators.
	GraceDays    int `mapstructure:"grace_days"`
	ReadOnlyDays int `mapstructure:"read_only_days"`
}

// AutomaticTax reports whether Stripe Tax is on.
func (b *BillingProviderConfig) AutomaticTax() bool {
	return b.StripeAutomaticTax == nil || *b.StripeAutomaticTax
}

// validateProvider checks the provider settings for provider ("" and "none": no payment provider).
func (b *BillingProviderConfig) validateProvider(provider string) error {
	if b.GraceDays == 0 {
		b.GraceDays = DefaultBillingGraceDays
	}
	if b.ReadOnlyDays == 0 {
		b.ReadOnlyDays = DefaultBillingReadOnlyDays
	}
	if b.GraceDays < 1 || b.GraceDays > maxBillingDunningDays || b.ReadOnlyDays < 1 || b.ReadOnlyDays > maxBillingDunningDays {
		return fmt.Errorf("[billing] grace_days and read_only_days must be between 1 and %d", maxBillingDunningDays)
	}
	switch provider {
	case "", "none":
		if b.StripeSecretKeyFile != "" || b.StripeWebhookSecretFile != "" {
			return errors.New(`[billing] stripe_* settings are given but provider is not "stripe"`)
		}
		return nil
	case BillingProviderStripe:
		if b.StripeSecretKeyFile == "" || b.StripeWebhookSecretFile == "" {
			return errors.New(`[billing] provider = "stripe" needs stripe_secret_key_file and stripe_webhook_secret_file`)
		}
		u, err := url.Parse(b.ReturnURL)
		if b.ReturnURL == "" || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return errors.New(`[billing] provider = "stripe" needs return_url, the absolute URL of the UI's billing page`)
		}
		return nil
	default:
		return fmt.Errorf("[billing] provider %q has no settings check", provider)
	}
}
