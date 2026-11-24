package chserver

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/capabilities"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/vault"
	"github.com/openrport/openrport/share/files"
	"github.com/openrport/openrport/share/tenant"
)

// staticVaultKeys is a managed vault key provider with one key for every tenant.
type staticVaultKeys struct{}

func (staticVaultKeys) Key(context.Context) ([]byte, error) {
	return bytes.Repeat([]byte{7}, vault.KeySize), nil
}
func (staticVaultKeys) Unlocked(context.Context) bool { return true }

func TestStatusCapabilitiesVaultPassphraseFollowsKeyProvider(t *testing.T) {
	for name, tc := range map[string]struct {
		keys     vault.KeyProvider
		override func(capabilities.Capabilities) capabilities.Capabilities
		want     string
	}{
		"passphrase":          {keys: nil, want: "true"},
		"managed":             {keys: staticVaultKeys{}, want: "false"},
		"override wins":       {keys: staticVaultKeys{}, want: "true", override: func(c capabilities.Capabilities) capabilities.Capabilities { c.VaultPassphrase = true; return c }},
		"override sees false": {keys: staticVaultKeys{}, want: "false", override: func(c capabilities.Capabilities) capabilities.Capabilities { return c }},
	} {
		t.Run(name, func(t *testing.T) {
			al := newStatusTestListener(&chconfig.Config{}, tc.override)
			al.vaultManager = newTestVaultManager(tc.keys)
			data := getStatusData(t, al)
			assert.Contains(t, string(data["capabilities"]), `"vault_passphrase":`+tc.want)
		})
	}
}

func newTestVaultManager(keys vault.KeyProvider) *vault.Manager {
	factory := vault.NewStatefulDbProviderFactory(
		func() (vault.DbProvider, error) { return &vault.NotInitDbProvider{}, nil },
		&vault.NotInitDbProvider{},
	)
	return vault.NewManagerWithKeyProvider(factory, &vault.Aes256PassManager{}, keys, testLog)
}

func TestVaultAdminEndpointsWithKeyProvider(t *testing.T) {
	al := newStatusTestListener(&chconfig.Config{}, nil)
	al.vaultManager = newTestVaultManager(staticVaultKeys{})
	ctx := tenant.WithID(context.Background(), tenant.Default)

	for name, handler := range map[string]http.HandlerFunc{
		"POST init":     al.handleVaultInit,
		"POST sesame":   al.handleVaultUnlock,
		"DELETE sesame": al.handleVaultLock,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"password":"12345678"}`)).WithContext(ctx)
		handler(rec, req)
		assert.Equal(t, http.StatusConflict, rec.Code, name)
		assert.Contains(t, rec.Body.String(), "the vault key is managed by the server", name)
	}

	rec := httptest.NewRecorder()
	al.handleGetVaultStatus(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"data":{"init":"setup-completed","status":"unlocked","managed":true}}`, rec.Body.String())
}

func TestNewServerUsesInjectedVaultKeys(t *testing.T) {
	s, err := NewServer(context.Background(), storageTestConfig(t), &ServerOpts{FilesAPI: files.NewFileSystem(), VaultKeys: staticVaultKeys{}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	ctx := tenant.WithID(context.Background(), tenant.Default)
	m := s.apiListener.vaultManager
	assert.False(t, m.UsesPassphrase())
	assert.False(t, s.apiListener.statusCapabilities().VaultPassphrase)

	// no init: the vault file is created on first use
	user := &users.User{Username: "admin"}
	id, err := m.Store(ctx, 0, &vault.InputValue{ClientID: "c1", Key: "k", Value: "v1", Type: vault.SecretType}, user)
	require.NoError(t, err)
	got, found, err := m.GetOne(ctx, int(id.ID), user)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "v1", got.Value)

	local, err := NewServer(context.Background(), storageTestConfig(t), &ServerOpts{FilesAPI: files.NewFileSystem()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = local.Close() })
	assert.True(t, local.apiListener.vaultManager.UsesPassphrase())
	assert.True(t, local.apiListener.statusCapabilities().VaultPassphrase)
}
