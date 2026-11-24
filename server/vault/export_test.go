package vault_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/vault"
	"github.com/openrport/openrport/share/enc"
)

func TestExportedValueEncryption(t *testing.T) {
	key := vault.PassphraseKey("my-pass")
	require.Len(t, key, vault.KeySize)

	// a value the passphrase vault wrote decrypts with the exported key and function
	legacy, err := enc.Aes256EncryptByPassToBase64String([]byte("secret one"), "my-pass")
	require.NoError(t, err)
	plain, err := vault.DecryptValue(legacy, key)
	require.NoError(t, err)
	assert.Equal(t, "secret one", string(plain))

	// and EncryptValue writes what the passphrase vault reads
	encoded, err := vault.EncryptValue([]byte("secret two"), key)
	require.NoError(t, err)
	plain, err = enc.Aes256DecryptByPassFromBase64String(encoded, "my-pass")
	require.NoError(t, err)
	assert.Equal(t, "secret two", string(plain))

	_, err = vault.DecryptValue(encoded, vault.PassphraseKey("other"))
	assert.Error(t, err)
	_, err = vault.EncryptValue([]byte("x"), []byte("short"))
	assert.Error(t, err)
	_, err = vault.DecryptValue(encoded, []byte("short"))
	assert.Error(t, err)
}
