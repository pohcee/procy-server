package vaultkms

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// localKeySize is the size of the key in a local key file (AES-256).
const localKeySize = 32

// localKeyVersion is the key version every response of the local KMS names.
const localKeyVersion = "/cryptoKeyVersions/1"

// localKMS is a KMS that wraps with one AES-256-GCM key read from a local file ([vault] key_provider = "local"),
// for self-hosted servers and tests. The key name of a request is not used: every name wraps with the file's key.
// A ciphertext is the GCM nonce followed by the sealed plaintext, bound to the additional authenticated data.
type localKMS struct {
	aead cipher.AEAD
}

var _ KMS = (*localKMS)(nil)

// NewLocalKMS returns a KMS wrapping with the 32-byte key in keyFile, hex or base64 encoded (surrounding whitespace
// is ignored).
func NewLocalKMS(keyFile string) (KMS, error) {
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("vault key file: %w", err)
	}
	key, err := decodeLocalKey(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("vault key file %s: %w", keyFile, err)
	}
	defer zero(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("vault key file %s: %w", keyFile, err)
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("vault key file %s: %w", keyFile, err)
	}
	return &localKMS{aead: g}, nil
}

func decodeLocalKey(s string) ([]byte, error) {
	if b, err := hex.DecodeString(s); err == nil && len(b) == localKeySize {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == localKeySize {
			return b, nil
		}
	}
	return nil, fmt.Errorf("expected a %d bytes key, hex or base64 encoded (e.g. openssl rand -hex 32)", localKeySize)
}

// checkCRC verifies an optional request checksum, as Cloud KMS does.
func checkCRC(sum *wrapperspb.Int64Value, b []byte, what string) (bool, error) {
	if sum == nil {
		return false, nil
	}
	if sum.GetValue() != crc(b) {
		return false, fmt.Errorf("local kms: the %s checksum does not match: the request was corrupted", what)
	}
	return true, nil
}

func (k *localKMS) Encrypt(_ context.Context, req *kmspb.EncryptRequest, _ ...gax.CallOption) (*kmspb.EncryptResponse, error) {
	ptOK, err := checkCRC(req.GetPlaintextCrc32C(), req.GetPlaintext(), "plaintext")
	if err != nil {
		return nil, err
	}
	adOK, err := checkCRC(req.GetAdditionalAuthenticatedDataCrc32C(), req.GetAdditionalAuthenticatedData(), "additional authenticated data")
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, k.aead.NonceSize(), k.aead.NonceSize()+len(req.GetPlaintext())+k.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("local kms: %w", err)
	}
	ct := k.aead.Seal(nonce, nonce, req.GetPlaintext(), req.GetAdditionalAuthenticatedData())
	return &kmspb.EncryptResponse{
		Name:                    req.GetName() + localKeyVersion,
		Ciphertext:              ct,
		CiphertextCrc32C:        wrapperspb.Int64(crc(ct)),
		VerifiedPlaintextCrc32C: ptOK,
		VerifiedAdditionalAuthenticatedDataCrc32C: adOK,
	}, nil
}

func (k *localKMS) Decrypt(_ context.Context, req *kmspb.DecryptRequest, _ ...gax.CallOption) (*kmspb.DecryptResponse, error) {
	if _, err := checkCRC(req.GetCiphertextCrc32C(), req.GetCiphertext(), "ciphertext"); err != nil {
		return nil, err
	}
	if _, err := checkCRC(req.GetAdditionalAuthenticatedDataCrc32C(), req.GetAdditionalAuthenticatedData(), "additional authenticated data"); err != nil {
		return nil, err
	}
	ct := req.GetCiphertext()
	if len(ct) < k.aead.NonceSize() {
		return nil, errors.New("local kms: the ciphertext is too short")
	}
	pt, err := k.aead.Open(nil, ct[:k.aead.NonceSize()], ct[k.aead.NonceSize():], req.GetAdditionalAuthenticatedData())
	if err != nil {
		return nil, errors.New("local kms: decryption failed: another key file or another tenant's key")
	}
	return &kmspb.DecryptResponse{Plaintext: pt, PlaintextCrc32C: wrapperspb.Int64(crc(pt))}, nil
}
