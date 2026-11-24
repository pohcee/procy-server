package vaultkms

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/openrport/openrport/share/tenant"
)

func writeKeyFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vault.key")
	require.NoError(t, os.WriteFile(path, []byte(content), 0600))
	return path
}

func TestLocalKMSRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	for name, content := range map[string]string{
		"hex":    hex.EncodeToString(key) + "\n",
		"base64": "  " + base64.StdEncoding.EncodeToString(key) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			k, err := NewLocalKMS(writeKeyFile(t, content))
			require.NoError(t, err)
			store := NewMemoryStore()
			p, err := New(Config{KMS: k, Store: store, KeyName: func(tenant.ID) (string, error) { return "local", nil }})
			require.NoError(t, err)
			ctx := tenant.WithID(context.Background(), "a")
			dek, err := p.Key(ctx)
			require.NoError(t, err)
			require.Len(t, dek, 32)

			// a new provider (another start) unwraps the stored key with the same key file
			k2, err := NewLocalKMS(writeKeyFile(t, content))
			require.NoError(t, err)
			p2, err := New(Config{KMS: k2, Store: store, KeyName: func(tenant.ID) (string, error) { return "local", nil }})
			require.NoError(t, err)
			again, err := p2.Key(ctx)
			require.NoError(t, err)
			assert.Equal(t, dek, again)

			// another key file can't unwrap it
			k3, err := NewLocalKMS(writeKeyFile(t, hex.EncodeToString(bytes.Repeat([]byte{8}, 32))))
			require.NoError(t, err)
			p3, err := New(Config{KMS: k3, Store: store, KeyName: func(tenant.ID) (string, error) { return "local", nil }})
			require.NoError(t, err)
			_, err = p3.Key(ctx)
			assert.Error(t, err)
		})
	}
}

func TestLocalKMSAADMismatchFails(t *testing.T) {
	k, err := NewLocalKMS(writeKeyFile(t, hex.EncodeToString(bytes.Repeat([]byte{7}, 32))))
	require.NoError(t, err)
	ctx := context.Background()
	pt, ad := []byte("plaintext key material"), []byte("tenant a")
	enc, err := k.Encrypt(ctx, &kmspb.EncryptRequest{
		Name: "local", Plaintext: pt, AdditionalAuthenticatedData: ad,
		PlaintextCrc32C: wrapperspb.Int64(crc(pt)), AdditionalAuthenticatedDataCrc32C: wrapperspb.Int64(crc(ad)),
	})
	require.NoError(t, err)
	assert.Equal(t, "local/cryptoKeyVersions/1", enc.GetName())
	assert.True(t, enc.GetVerifiedPlaintextCrc32C())
	assert.True(t, enc.GetVerifiedAdditionalAuthenticatedDataCrc32C())
	assert.Equal(t, crc(enc.GetCiphertext()), enc.GetCiphertextCrc32C().GetValue())

	dec, err := k.Decrypt(ctx, &kmspb.DecryptRequest{Name: "local", Ciphertext: enc.GetCiphertext(), AdditionalAuthenticatedData: ad})
	require.NoError(t, err)
	assert.Equal(t, pt, dec.GetPlaintext())
	assert.Equal(t, crc(pt), dec.GetPlaintextCrc32C().GetValue())

	_, err = k.Decrypt(ctx, &kmspb.DecryptRequest{Name: "local", Ciphertext: enc.GetCiphertext(), AdditionalAuthenticatedData: []byte("tenant b")})
	assert.Error(t, err, "bound to the additional authenticated data")

	_, err = k.Encrypt(ctx, &kmspb.EncryptRequest{Name: "local", Plaintext: pt, PlaintextCrc32C: wrapperspb.Int64(crc(pt) + 1)})
	assert.Error(t, err, "a corrupted request is refused")
}

func TestLocalKMSBadKeyFile(t *testing.T) {
	_, err := NewLocalKMS(filepath.Join(t.TempDir(), "missing"))
	assert.Error(t, err)

	_, err = NewLocalKMS(writeKeyFile(t, hex.EncodeToString(bytes.Repeat([]byte{7}, 16))))
	assert.ErrorContains(t, err, "32 bytes")

	_, err = NewLocalKMS(writeKeyFile(t, "not a key"))
	assert.ErrorContains(t, err, "32 bytes")

	_, err = NewLocalKMS(writeKeyFile(t, ""))
	assert.Error(t, err)
}
