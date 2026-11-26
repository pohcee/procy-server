package users

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/chconfig"
	"github.com/pohcee/procy-server/share/enums"
)

func TestNewProviderFromConfigStatic(t *testing.T) {
	config := &chconfig.Config{API: chconfig.APIConfig{Auth: "admin:foo"}}

	p, err := NewProviderFromConfig(defaultCtx, nil, config)
	require.NoError(t, err)
	require.NotNil(t, p)

	user, err := p.GetByUsername(defaultCtx, "admin")
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, []string{Administrators}, user.Groups)
}

func TestNewProviderFromConfigFile(t *testing.T) {
	authFile := filepath.Join(t.TempDir(), "auth.json")
	hash, err := GenerateTokenHash("foo")
	require.NoError(t, err)
	usersJSON := `[{"username":"admin","password":"` + hash + `","groups":["Administrators"]}]`
	require.NoError(t, os.WriteFile(authFile, []byte(usersJSON), 0600))
	config := &chconfig.Config{API: chconfig.APIConfig{AuthFile: authFile}}

	p, err := NewProviderFromConfig(defaultCtx, nil, config)
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, enums.ProviderSourceFile, p.Type())
}

func TestNewProviderFromConfigNone(t *testing.T) {
	p, err := NewProviderFromConfig(defaultCtx, nil, &chconfig.Config{})
	require.NoError(t, err)
	assert.Nil(t, p)
}
