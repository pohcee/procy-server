package vault

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/share/enc"
	"github.com/pohcee/procy-server/share/tenant"
)

func TestPassphraseKeyMatchesLegacyFormat(t *testing.T) {
	ctx := tenant.WithID(context.Background(), tenant.Default)
	p := NewPassphraseKeyProvider()
	p.setPass(tenant.Default, "my-pass")
	key, err := p.Key(ctx)
	require.NoError(t, err)
	require.Len(t, key, KeySize)

	// a value written by earlier versions decrypts with the provider's key
	legacy, err := enc.Aes256EncryptByPassToBase64String([]byte("secret one"), "my-pass")
	require.NoError(t, err)
	plain, err := decryptValue(legacy, key)
	require.NoError(t, err)
	assert.Equal(t, "secret one", string(plain))

	// and a value written now decrypts the way earlier versions read it
	current, err := encryptValue([]byte("secret two"), key)
	require.NoError(t, err)
	plain, err = enc.Aes256DecryptByPassFromBase64String(current, "my-pass")
	require.NoError(t, err)
	assert.Equal(t, "secret two", string(plain))
}

func TestPassphraseKeyLocked(t *testing.T) {
	a := tenant.WithID(context.Background(), "a")
	b := tenant.WithID(context.Background(), "b")
	p := NewPassphraseKeyProvider()

	_, err := p.Key(a)
	assert.Equal(t, errVaultLocked, err)
	assert.False(t, p.Unlocked(a))

	p.setPass("a", "pass-a")
	assert.True(t, p.Unlocked(a))
	assert.False(t, p.Unlocked(b))
	_, err = p.Key(b)
	assert.Equal(t, errVaultLocked, err)

	p.setPass("a", "")
	assert.False(t, p.Unlocked(a))

	_, err = p.Key(context.Background())
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.False(t, p.Unlocked(context.Background()))
}

func TestValueKeyLengthChecked(t *testing.T) {
	_, err := encryptValue([]byte("x"), []byte("short"))
	assert.ErrorContains(t, err, "5-byte key")
	_, err = decryptValue("AAAA", make([]byte, 16))
	assert.ErrorContains(t, err, "16-byte key")
}
