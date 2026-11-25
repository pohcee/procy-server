package chconfig

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeteringNeedsPostgres(t *testing.T) {
	err := validate(&Config{Server: defaultValidMinServerConfig, Metering: MeteringConfig{Enabled: true}})
	assert.ErrorContains(t, err, `[metering] needs [storage] backend = "postgres"`)
	err = validate(&Config{Server: defaultValidMinServerConfig, Metering: MeteringConfig{FlushInterval: time.Minute}})
	assert.ErrorContains(t, err, "enabled is false")
}

func TestMeteringDefaultsAndBounds(t *testing.T) {
	c := postgresConfig()
	c.Metering = MeteringConfig{Enabled: true}
	require.NoError(t, validate(c))
	assert.Equal(t, DefaultMeteringFlushInterval, c.Metering.FlushInterval)

	for _, m := range []MeteringConfig{
		{Enabled: true, FlushInterval: time.Second},
		{Enabled: true, FlushInterval: time.Hour},
		{Enabled: true, HourlyRetention: 24 * time.Hour},
		{Enabled: true, DailyRetention: -time.Hour},
		{Enabled: true, DailyRetention: 30 * 24 * time.Hour}, // shorter than the default 90 days of hours
	} {
		c := postgresConfig()
		c.Metering = m
		assert.Error(t, validate(c), "%+v", m)
	}
}
