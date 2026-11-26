package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/chconfig"
	"github.com/pohcee/procy-server/share/logger"
)

func writeMigrateConfig(t *testing.T, dataDir string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rportd.conf")
	conf := "[server]\n" +
		"  auth = \"client:secret\"\n" +
		"  data_dir = \"" + dataDir + "\"\n"
	require.NoError(t, os.WriteFile(path, []byte(conf), 0600))
	prev := *cfgPath
	*cfgPath = path
	t.Cleanup(func() { *cfgPath = prev })
}

func asRoot(t *testing.T) {
	t.Helper()
	prev := isRunningAsRoot
	isRunningAsRoot = func() bool { return true }
	t.Cleanup(func() { isRunningAsRoot = prev })
}

func TestMigrateRefusesRoot(t *testing.T) {
	asRoot(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	writeMigrateConfig(t, dataDir)

	err := runMigrate(logger.NewLogger("migrate", logger.LogOutput{File: os.Stdout}, logger.LogLevelInfo))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "root")
	assert.NoDirExists(t, dataDir, "nothing may be created as root")
}

// The allow_root cases are checked on rootRefused directly.
func TestRootRefused(t *testing.T) {
	asRoot(t)
	assert.True(t, rootRefused(&chconfig.Config{}))
	assert.False(t, rootRefused(&chconfig.Config{Server: chconfig.ServerConfig{AllowRoot: true}}))

	isRunningAsRoot = func() bool { return false }
	assert.False(t, rootRefused(&chconfig.Config{}))
}
