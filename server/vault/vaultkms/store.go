package vaultkms

import (
	"bytes"
	"context"
	"sync"

	"github.com/pohcee/procy-server/share/tenant"
)

// WrappedKey is a tenant's data encryption key (DEK) as stored: encrypted by a Cloud KMS key.
type WrappedKey struct {
	// KeyName is the resource name of the KMS CryptoKey that wrapped the DEK
	// (projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>). Unwrapping uses it, so the key that wraps new DEKs
	// can change without touching the DEKs already stored.
	KeyName string
	// Ciphertext is the wrapped DEK as KMS returned it.
	Ciphertext []byte
	// Shredded marks a tombstone: the tenant's DEK was destroyed and must not be created again.
	Shredded bool
}

func (w WrappedKey) equal(o WrappedKey) bool {
	return w.KeyName == o.KeyName && bytes.Equal(w.Ciphertext, o.Ciphertext) && w.Shredded == o.Shredded
}

// WrappedKeyStore keeps one wrapped DEK per tenant. The Postgres storage implements it (storage/postgres/vaultkeys); every server instance must
// see the same store.
type WrappedKeyStore interface {
	// Get returns the tenant's wrapped DEK; found is false when there is none.
	Get(ctx context.Context, tid tenant.ID) (key WrappedKey, found bool, err error)
	// CreateIfAbsent stores key for the tenant unless it already has one (a tombstone counts), and returns the one
	// stored. When two instances create a DEK at the same time, both get the winner's (INSERT ... ON CONFLICT DO
	// NOTHING, then SELECT).
	CreateIfAbsent(ctx context.Context, tid tenant.ID, key WrappedKey) (stored WrappedKey, err error)
	// Shred replaces the tenant's wrapped DEK with a tombstone (Shredded, no ciphertext), creating the tombstone when
	// there is no key. Without the wrapped DEK the tenant's vault values cannot be decrypted any more
	// (crypto-shredding); the tombstone stops any instance from creating a new DEK for the tenant.
	Shred(ctx context.Context, tid tenant.ID) error
}

// MemoryStore is a WrappedKeyStore in memory, for tests and single-process use.
type MemoryStore struct {
	mu   sync.Mutex
	keys map[tenant.ID]WrappedKey
}

var _ WrappedKeyStore = (*MemoryStore)(nil)

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{keys: map[tenant.ID]WrappedKey{}}
}

func (s *MemoryStore) Get(_ context.Context, tid tenant.ID) (WrappedKey, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[tid]
	return cloneWrapped(k), ok, nil
}

func (s *MemoryStore) CreateIfAbsent(_ context.Context, tid tenant.ID, key WrappedKey) (WrappedKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.keys[tid]; ok {
		return cloneWrapped(existing), nil
	}
	s.keys[tid] = cloneWrapped(key)
	return cloneWrapped(key), nil
}

func (s *MemoryStore) Shred(_ context.Context, tid tenant.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[tid] = WrappedKey{Shredded: true}
	return nil
}

func cloneWrapped(k WrappedKey) WrappedKey {
	return WrappedKey{KeyName: k.KeyName, Ciphertext: bytes.Clone(k.Ciphertext), Shredded: k.Shredded}
}
