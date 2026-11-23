package chconfig

import "errors"

// LibraryConfig configures the script and command library ([library] section).
type LibraryConfig struct {
	// KeepVersions keeps only the newest N versions of each entry. 0 = keep all.
	KeepVersions int `mapstructure:"keep_versions"`
	// KeepVersionsDays removes versions older than N days. 0 = no age limit.
	KeepVersionsDays int `mapstructure:"keep_versions_days"`
}

func (c *Config) parseAndValidateLibrary() error {
	if c.Library.KeepVersions < 0 {
		return errors.New("keep_versions must not be negative")
	}
	if c.Library.KeepVersionsDays < 0 {
		return errors.New("keep_versions_days must not be negative")
	}
	return nil
}
