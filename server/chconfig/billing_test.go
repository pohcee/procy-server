package chconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBillingNeedsPostgresAndMetering(t *testing.T) {
	err := validate(&Config{Server: defaultValidMinServerConfig, Billing: BillingConfig{Enabled: true, PlansFile: "p.toml"}})
	assert.ErrorContains(t, err, `[billing] needs [storage] backend = "postgres"`)
	err = validate(&Config{Server: defaultValidMinServerConfig, Billing: BillingConfig{PlansFile: "p.toml"}})
	assert.ErrorContains(t, err, "enabled is false")

	c := postgresConfig()
	c.Billing = BillingConfig{Enabled: true, PlansFile: "p.toml"}
	assert.ErrorContains(t, validate(c), "needs [metering] enabled")
}

func TestBillingDefaultsAndBounds(t *testing.T) {
	c := postgresConfig()
	c.Metering = MeteringConfig{Enabled: true}
	c.Billing = BillingConfig{Enabled: true, PlansFile: "p.toml", OverageReviewAbove: 100}
	require.NoError(t, validate(c))
	assert.Equal(t, "none", c.Billing.Provider)

	for _, b := range []BillingConfig{
		{Enabled: true},
		{Enabled: true, PlansFile: "p.toml", Provider: "paypal"},
		{Enabled: true, PlansFile: "p.toml", OverageReviewAbove: -1},
		{Enabled: true, PlansFile: "p.toml", ImplausibleGrowthFactor: 1.5},
	} {
		c := postgresConfig()
		c.Metering = MeteringConfig{Enabled: true}
		c.Billing = b
		assert.Error(t, validate(c), "%+v", b)
	}
}
