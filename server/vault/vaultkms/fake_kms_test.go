package vaultkms

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// fakeKMS acts like Cloud KMS symmetric encryption: each key name has its own AES-GCM key, the additional
// authenticated data is bound to the ciphertext, and the CRC32C fields are verified and returned.
type fakeKMS struct {
	encrypts, decrypts atomic.Int32

	mu  sync.Mutex
	err error
	// corruptEncryptCRC / corruptDecryptCRC make the responses fail their integrity checks.
	corruptEncryptCRC, corruptDecryptCRC bool
	// unverified makes Encrypt answer without verifying the request checksums.
	unverified bool
	// beforeEncrypt / beforeDecrypt, when set, run at the start of every Encrypt / Decrypt.
	beforeEncrypt, beforeDecrypt func()
	// wrongName makes Encrypt answer for another key.
	wrongName bool
}

var _ KMS = (*fakeKMS)(nil)

func (f *fakeKMS) aead(name string) cipher.AEAD {
	sum := sha256.Sum256([]byte("fake-kms:" + name))
	block, _ := aes.NewCipher(sum[:])
	g, _ := cipher.NewGCM(block)
	return g
}

func (f *fakeKMS) settings() (bool, bool, bool, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.corruptEncryptCRC, f.corruptDecryptCRC, f.unverified, f.beforeEncrypt, f.err
}

func (f *fakeKMS) Encrypt(_ context.Context, req *kmspb.EncryptRequest, _ ...gax.CallOption) (*kmspb.EncryptResponse, error) {
	f.encrypts.Add(1)
	corrupt, _, unverified, before, err := f.settings()
	if before != nil {
		before()
	}
	if err != nil {
		return nil, err
	}
	if req.GetPlaintextCrc32C() == nil || req.GetPlaintextCrc32C().GetValue() != crc(req.GetPlaintext()) {
		return nil, errors.New("fake kms: plaintext checksum missing or wrong")
	}
	if req.GetAdditionalAuthenticatedDataCrc32C() == nil || req.GetAdditionalAuthenticatedDataCrc32C().GetValue() != crc(req.GetAdditionalAuthenticatedData()) {
		return nil, errors.New("fake kms: aad checksum missing or wrong")
	}
	g := f.aead(req.GetName())
	nonce := make([]byte, g.NonceSize())
	_, _ = rand.Read(nonce)
	ct := g.Seal(nonce, nonce, req.GetPlaintext(), req.GetAdditionalAuthenticatedData())
	sum := crc(ct)
	if corrupt {
		sum++
	}
	return &kmspb.EncryptResponse{
		Name:                    f.responseName(req.GetName()),
		Ciphertext:              ct,
		CiphertextCrc32C:        wrapperspb.Int64(sum),
		VerifiedPlaintextCrc32C: !unverified,
		VerifiedAdditionalAuthenticatedDataCrc32C: !unverified,
	}, nil
}

func (f *fakeKMS) Decrypt(_ context.Context, req *kmspb.DecryptRequest, _ ...gax.CallOption) (*kmspb.DecryptResponse, error) {
	f.decrypts.Add(1)
	f.mu.Lock()
	before := f.beforeDecrypt
	f.mu.Unlock()
	if before != nil {
		before()
	}
	_, corrupt, _, _, err := f.settings()
	if err != nil {
		return nil, err
	}
	if req.GetCiphertextCrc32C() == nil || req.GetCiphertextCrc32C().GetValue() != crc(req.GetCiphertext()) {
		return nil, errors.New("fake kms: ciphertext checksum missing or wrong")
	}
	if req.GetAdditionalAuthenticatedDataCrc32C() == nil || req.GetAdditionalAuthenticatedDataCrc32C().GetValue() != crc(req.GetAdditionalAuthenticatedData()) {
		return nil, errors.New("fake kms: aad checksum missing or wrong")
	}
	g := f.aead(req.GetName())
	ct := req.GetCiphertext()
	if len(ct) < g.NonceSize() {
		return nil, errors.New("fake kms: ciphertext too short")
	}
	pt, err := g.Open(nil, ct[:g.NonceSize()], ct[g.NonceSize():], req.GetAdditionalAuthenticatedData())
	if err != nil {
		return nil, errors.New("fake kms: decryption failed")
	}
	sum := crc(pt)
	if corrupt {
		sum++
	}
	return &kmspb.DecryptResponse{Plaintext: pt, PlaintextCrc32C: wrapperspb.Int64(sum)}, nil
}

func (f *fakeKMS) responseName(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.wrongName {
		return name + "-other/cryptoKeyVersions/1"
	}
	return name + "/cryptoKeyVersions/1"
}
