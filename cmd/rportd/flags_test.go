package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestFlags(t *testing.T, args ...string) (*viper.Viper, *pflag.FlagSet) {
	t.Helper()
	fs := pflag.NewFlagSet("rportd", pflag.ContinueOnError)
	addServerFlags(fs)
	require.NoError(t, fs.Parse(args))
	v := viper.New()
	v.SetConfigType("toml")
	return v, fs
}

func TestBindPFlagsFindsEveryFlag(t *testing.T) {
	v, fs := newTestFlags(t)
	assert.NoError(t, bindPFlags(v, fs))
}

func TestBindPFlagsRefusesUnknownFlag(t *testing.T) {
	v := viper.New()
	fs := pflag.NewFlagSet("rportd", pflag.ContinueOnError)
	assert.Error(t, bindPFlags(v, fs), "an empty flag set has none of the bound flags")
}

func TestFlagOverridesConfigFile(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "rportd.conf")
	require.NoError(t, os.WriteFile(conf, []byte("[server]\n  data_dir = \"/from/config\"\n"), 0600))

	v, fs := newTestFlags(t, "--data-dir", "/from/flag", "--allow-root", "--api-addr", "127.0.0.1:3001")
	require.NoError(t, bindPFlags(v, fs))
	v.SetConfigFile(conf)
	require.NoError(t, v.ReadInConfig())

	assert.Equal(t, "/from/flag", v.GetString("server.data_dir"))
	assert.True(t, v.GetBool("server.allow_root"))
	assert.Equal(t, "127.0.0.1:3001", v.GetString("api.address"))
}

func TestUnsetFlagKeepsConfigFileAndDefaults(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "rportd.conf")
	require.NoError(t, os.WriteFile(conf, []byte("[server]\n  data_dir = \"/from/config\"\n"), 0600))

	v, fs := newTestFlags(t)
	v.SetDefault("server.auth_write", true)
	v.SetDefault("server.address", "0.0.0.0:8080")
	require.NoError(t, bindPFlags(v, fs))
	v.SetConfigFile(conf)
	require.NoError(t, v.ReadInConfig())

	assert.Equal(t, "/from/config", v.GetString("server.data_dir"))
	assert.True(t, v.GetBool("server.auth_write"), "an unset bool flag must not override the default")
	assert.Equal(t, "0.0.0.0:8080", v.GetString("server.address"), "an unset flag must not override the default")
}
