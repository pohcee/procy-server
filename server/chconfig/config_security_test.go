package chconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// apiConfigWith returns a minimal valid config with the API enabled, using dataDir.
func apiConfigWith(dataDir string, api APIConfig) Config {
	api.Address = "0.0.0.0:3000"
	if api.Auth == "" && api.AuthFile == "" && api.AuthUserTable == "" {
		api.Auth = "admin:a-real-password"
	}
	server := defaultValidMinServerConfig
	server.DataDir = dataDir
	return Config{Server: server, API: api}
}

func TestRejectsPlaceholderAndExampleJWTSecrets(t *testing.T) {
	for _, secret := range []string{"<YOUR_SECRET>", "quei1too2Jae3xootu"} {
		t.Run(secret, func(t *testing.T) {
			cfg := apiConfigWith(t.TempDir(), APIConfig{JWTSecret: secret})
			err := cfg.ParseAndValidate(&Mlog)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "jwt_secret")
			assert.Contains(t, err.Error(), "example")
		})
	}
}

func TestRejectsTooShortJWTSecret(t *testing.T) {
	cfg := apiConfigWith(t.TempDir(), APIConfig{JWTSecret: "short-secret"})
	err := cfg.ParseAndValidate(&Mlog)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "jwt_secret is too short")
}

func TestAcceptsCustomJWTSecretOfMinimumLength(t *testing.T) {
	// 18 characters: existing installs with a custom secret of this length must keep starting.
	cfg := apiConfigWith(t.TempDir(), APIConfig{JWTSecret: "Xk2pQ9rT4vW7yZ1bN3"})
	require.NoError(t, cfg.ParseAndValidate(&Mlog))
	assert.Equal(t, "Xk2pQ9rT4vW7yZ1bN3", cfg.API.JWTSecret)
}

func TestGeneratedJWTSecretIsPersistedAndReused(t *testing.T) {
	dataDir := t.TempDir()

	first := apiConfigWith(dataDir, APIConfig{})
	require.NoError(t, first.ParseAndValidate(&Mlog))
	assert.GreaterOrEqual(t, len(first.API.JWTSecret), 32, "generated secret should be at least 32 characters")

	info, err := os.Stat(filepath.Join(dataDir, jwtSecretFileName))
	require.NoError(t, err, "generated secret should be saved in the data dir")
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())

	second := apiConfigWith(dataDir, APIConfig{})
	require.NoError(t, second.ParseAndValidate(&Mlog))
	assert.Equal(t, first.API.JWTSecret, second.API.JWTSecret, "a restart must reuse the saved secret, not log everyone out")
}

func TestRejectsPlaceholderKeySeed(t *testing.T) {
	cfg := Config{Server: defaultValidMinServerConfig}
	cfg.Server.KeySeed = "<YOUR_SEED>"
	err := cfg.ParseAndValidate(&Mlog)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key_seed")
}

func TestRejectsExampleAPICredentials(t *testing.T) {
	cfg := apiConfigWith(t.TempDir(), APIConfig{Auth: "admin:foobaz", JWTSecret: "Xk2pQ9rT4vW7yZ1bN3"})
	err := cfg.ParseAndValidate(&Mlog)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "example")
}

func TestRejectsExampleClientCredentials(t *testing.T) {
	cfg := Config{Server: defaultValidMinServerConfig}
	cfg.Server.Auth = "clientAuth1:1234"
	err := cfg.ParseAndValidate(&Mlog)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "example")
}

func TestTrustedProxiesAreParsed(t *testing.T) {
	cfg := apiConfigWith(t.TempDir(), APIConfig{TrustedProxies: []string{"10.0.0.0/8", "192.0.2.1"}})
	require.NoError(t, cfg.ParseAndValidate(&Mlog))
	require.Len(t, cfg.API.ParsedTrustedProxies, 2)
	assert.Equal(t, "10.0.0.0/8", cfg.API.ParsedTrustedProxies[0].String())
}

func TestInvalidTrustedProxyIsRejected(t *testing.T) {
	cfg := apiConfigWith(t.TempDir(), APIConfig{TrustedProxies: []string{"proxy.example.com"}})
	err := cfg.ParseAndValidate(&Mlog)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trusted proxy")
}

func TestHeaderAuthRequiresTrustedProxies(t *testing.T) {
	cfg := apiConfigWith(t.TempDir(), APIConfig{
		AuthHeader: "Authentication-IsAuthenticated", UserHeader: "Authentication-User",
		AuthFile: "users.json", Auth: "-",
	})
	cfg.API.Auth = ""
	err := cfg.ParseAndValidate(&Mlog)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trusted_proxies")

	cfg = apiConfigWith(t.TempDir(), APIConfig{
		AuthHeader: "Authentication-IsAuthenticated", UserHeader: "Authentication-User",
		AuthFile: "users.json", TrustedProxies: []string{"10.0.0.0/8"},
	})
	cfg.API.Auth = ""
	assert.NoError(t, cfg.ParseAndValidate(&Mlog))
}
