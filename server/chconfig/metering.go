package chconfig

import (
	"errors"
	"fmt"
	"time"
)

// Metering defaults and bounds.
const (
	DefaultMeteringFlushInterval = time.Minute
	minMeteringFlushInterval     = 5 * time.Second
	maxMeteringFlushInterval     = 30 * time.Minute
	minUsageHourlyRetention      = 10 * 24 * time.Hour
)

// MeteringConfig is the [metering] section: counting what each organization uses (tunnel and upload bytes, API
// bytes, jobs, connected devices) per hour. Postgres only; absent (off) by default.
type MeteringConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// FlushInterval is how often each process writes its counts (default 1 minute).
	FlushInterval time.Duration `mapstructure:"flush_interval"`
	// HourlyRetention is how long the hourly usage (and the per-device daily transfer) is kept (default 90 days,
	// at least 10).
	HourlyRetention time.Duration `mapstructure:"hourly_retention"`
	// DailyRetention is how long the daily usage and the device-days are kept (default 25 months).
	DailyRetention time.Duration `mapstructure:"daily_retention"`
}

func (c *Config) parseAndValidateMetering() error {
	m := &c.Metering
	if !m.Enabled {
		if m.FlushInterval != 0 || m.HourlyRetention != 0 || m.DailyRetention != 0 {
			return errors.New("[metering] settings are given but enabled is false")
		}
		return nil
	}
	if !c.Storage.IsPostgres() {
		return errors.New(`[metering] needs [storage] backend = "postgres"`)
	}
	switch {
	case m.FlushInterval == 0:
		m.FlushInterval = DefaultMeteringFlushInterval
	case m.FlushInterval < minMeteringFlushInterval || m.FlushInterval > maxMeteringFlushInterval:
		return fmt.Errorf("[metering] flush_interval must be between %s and %s, it is %s", minMeteringFlushInterval, maxMeteringFlushInterval, m.FlushInterval)
	}
	if m.HourlyRetention < 0 || (m.HourlyRetention > 0 && m.HourlyRetention < minUsageHourlyRetention) {
		return fmt.Errorf("[metering] hourly_retention must be at least %s (hours are rolled up into days for 9 days), it is %s", minUsageHourlyRetention, m.HourlyRetention)
	}
	if m.DailyRetention < 0 {
		return fmt.Errorf("[metering] daily_retention must not be negative")
	}
	hourly := m.HourlyRetention
	if hourly == 0 {
		hourly = 90 * 24 * time.Hour
	}
	if m.DailyRetention > 0 && m.DailyRetention < hourly {
		return fmt.Errorf("[metering] daily_retention (%s) must be at least hourly_retention (%s)", m.DailyRetention, hourly)
	}
	return nil
}
