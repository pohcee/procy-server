package chconfig

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// BillingProviders are the payment providers [billing] provider may name. "none" (the default) has plans, limits
// and usage without payments; a provider implementation adds its name.
var BillingProviders = map[string]bool{"none": true}

// BillingConfig is the [billing] section: plans from a private plans file, a billing status per organization,
// monthly billing periods and transfer limits. Postgres only, needs [metering]; absent (off) by default.
type BillingConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// Provider is the payment provider (default "none").
	Provider string `mapstructure:"provider"`
	// PlansFile is the plans file (TOML, kept private: names, prices, allowances, rates).
	PlansFile string `mapstructure:"plans_file"`
	// EgressCostPerGiB is for the operators' estimated cost only (USD).
	EgressCostPerGiB float64 `mapstructure:"egress_cost_per_gib"`
	// OverageReviewAbove holds a closed period whose overage exceeds it (USD) for an operator's approval; 0: no hold.
	OverageReviewAbove float64 `mapstructure:"overage_review_above"`
	// ImplausibleGrowthFactor: the hard cap is not applied to an organization whose last hour's transfer is more
	// than this many times its average hour of the period (a counting bug); default 20.
	ImplausibleGrowthFactor float64 `mapstructure:"implausible_growth_factor"`
	// BillingProviderConfig holds the payment provider's settings (billing_provider.go).
	BillingProviderConfig `mapstructure:",squash"`
}

func (c *Config) parseAndValidateBilling() error {
	b := &c.Billing
	if !b.Enabled {
		if b.Provider != "" || b.PlansFile != "" || b.EgressCostPerGiB != 0 || b.OverageReviewAbove != 0 || b.ImplausibleGrowthFactor != 0 ||
			b.BillingProviderConfig != (BillingProviderConfig{}) {
			return errors.New("[billing] settings are given but enabled is false")
		}
		return nil
	}
	if !c.Storage.IsPostgres() {
		return errors.New(`[billing] needs [storage] backend = "postgres"`)
	}
	if !c.Metering.Enabled {
		return errors.New("[billing] needs [metering] enabled: plans and limits are computed from the metered usage")
	}
	if b.Provider == "" {
		b.Provider = "none"
	}
	if !BillingProviders[b.Provider] {
		names := make([]string, 0, len(BillingProviders))
		for n := range BillingProviders {
			names = append(names, fmt.Sprintf("%q", n))
		}
		sort.Strings(names)
		return fmt.Errorf("[billing] provider must be one of %s, not %q", strings.Join(names, ", "), b.Provider)
	}
	if b.PlansFile == "" {
		return errors.New("[billing] plans_file is required")
	}
	for name, v := range map[string]float64{"egress_cost_per_gib": b.EgressCostPerGiB, "overage_review_above": b.OverageReviewAbove, "implausible_growth_factor": b.ImplausibleGrowthFactor} {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("[billing] %s must not be negative", name)
		}
	}
	if b.ImplausibleGrowthFactor != 0 && b.ImplausibleGrowthFactor < 2 {
		return errors.New("[billing] implausible_growth_factor must be at least 2")
	}
	return b.validateProvider(b.Provider)
}
