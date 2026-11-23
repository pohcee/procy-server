package chconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLibraryConfigRejectsNegative(t *testing.T) {
	c := &Config{Library: LibraryConfig{KeepVersions: -1}}
	err := c.parseAndValidateLibrary()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keep_versions")

	c = &Config{Library: LibraryConfig{KeepVersionsDays: -1}}
	err = c.parseAndValidateLibrary()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keep_versions_days")

	c = &Config{Library: LibraryConfig{KeepVersions: 10, KeepVersionsDays: 30}}
	assert.NoError(t, c.parseAndValidateLibrary())
	assert.NoError(t, (&Config{}).parseAndValidateLibrary())
}
