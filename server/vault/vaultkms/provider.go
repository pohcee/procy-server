// Package vaultkms is a managed vault key provider: envelope encryption with a key management service (Cloud KMS,
// or a local key file for self-hosted servers), for servers on Postgres ([vault] key_provider).
//
// Each tenant has its own 256-bit data encryption key (DEK). The OSS vault encrypts the tenant's values with it
// (AES-256-GCM). The DEK is created on the tenant's first vault use, wrapped (encrypted) by a Cloud KMS key with the
// tenant ID as additional authenticated data, and only the wrapped DEK is stored (WrappedKeyStore). A server
// unwraps it on first use and keeps it in memory for a short TTL, so restarts and new instances need no passphrase.
// Shred replaces the wrapped DEK with a tombstone, which makes the tenant's vault values unreadable
// (crypto-shredding) and stops every instance from creating a new DEK for the tenant.
//
// Plug it into the OSS server with chserver.ServerOpts{VaultKeys: provider}.
package vaultkms

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/googleapis/gax-go/v2"
	"golang.org/x/sync/singleflight"
	"google.golang.org/protobuf/types/known/wrapperspb"

	errors2 "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/server/vault"
	"github.com/openrport/openrport/share/tenant"
)

// DefaultTTL is how long an unwrapped DEK stays in memory when Config.TTL is 0.
const DefaultTTL = 5 * time.Minute

// kmsTimeout bounds one load of a DEK (store and KMS calls). The load is detached from the caller's cancellation,
// because concurrent callers for the same tenant share it.
const kmsTimeout = 30 * time.Second

// KMS is the part of the Cloud KMS client the provider uses. *kms.KeyManagementClient (cloud.google.com/go/kms/apiv1)
// satisfies it.
type KMS interface {
	Encrypt(ctx context.Context, req *kmspb.EncryptRequest, opts ...gax.CallOption) (*kmspb.EncryptResponse, error)
	Decrypt(ctx context.Context, req *kmspb.DecryptRequest, opts ...gax.CallOption) (*kmspb.DecryptResponse, error)
}

// Config configures a Provider. KMS, Store and KeyName are required.
type Config struct {
	KMS   KMS
	Store WrappedKeyStore
	// KeyName returns the resource name of the KMS CryptoKey that wraps a new DEK for the tenant
	// (projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>): one key per tenant, or one per region.
	KeyName func(tid tenant.ID) (string, error)
	// TTL is how long an unwrapped DEK is kept in memory; 0 means DefaultTTL.
	TTL time.Duration
	// OnError, when set, receives the cause of every failure to get a tenant's key. The API caller only gets a
	// generic 503, so this is where the KMS or store error is logged.
	OnError func(tid tenant.ID, err error)
	// Now and Rand default to time.Now and crypto/rand.Reader.
	Now  func() time.Time
	Rand io.Reader
}

// Provider is a vault.KeyProvider backed by Cloud KMS envelope encryption. It is safe for concurrent use.
type Provider struct {
	cfg   Config
	mu    sync.Mutex
	cache map[tenant.ID]cachedKey
	// shreds counts the Shred calls per tenant on this instance: a load that saw another count caches nothing.
	shreds map[tenant.ID]uint64
	loads  singleflight.Group
}

type cachedKey struct {
	dek     []byte
	expires time.Time
}

var _ vault.KeyProvider = (*Provider)(nil)

// ErrShredded is what the vault API caller gets for a tenant whose key was destroyed.
var ErrShredded = errors2.APIError{
	Message:    "the vault key of this tenant was destroyed",
	HTTPStatus: http.StatusGone,
}

var errShredded = errors.New("vaultkms: the tenant's key was shredded")

// ErrKeyUnavailable is what the vault API caller gets when the tenant's key cannot be loaded.
var ErrKeyUnavailable = errors2.APIError{
	Message:    "the vault key is not available, try again later",
	HTTPStatus: http.StatusServiceUnavailable,
}

func New(cfg Config) (*Provider, error) {
	switch {
	case cfg.KMS == nil:
		return nil, errors.New("vaultkms: KMS is required")
	case cfg.Store == nil:
		return nil, errors.New("vaultkms: Store is required")
	case cfg.KeyName == nil:
		return nil, errors.New("vaultkms: KeyName is required")
	case cfg.TTL < 0:
		return nil, errors.New("vaultkms: TTL must not be negative")
	}
	if cfg.TTL == 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	return &Provider{cfg: cfg, cache: map[tenant.ID]cachedKey{}, shreds: map[tenant.ID]uint64{}}, nil
}

// Key returns the DEK of ctx's tenant, creating it on the tenant's first use.
func (p *Provider) Key(ctx context.Context) ([]byte, error) {
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("vaultkms: %w", err)
	}
	if dek, ok := p.cached(tid); ok {
		return dek, nil
	}

	gen := p.shredCount(tid)
	v, err, _ := p.loads.Do(string(tid), func() (interface{}, error) {
		if dek, ok := p.cached(tid); ok {
			return dek, nil
		}
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), kmsTimeout)
		defer cancel()
		dek, err := p.load(loadCtx, tid)
		if err != nil {
			if p.cfg.OnError != nil && !errors.Is(err, errShredded) {
				p.cfg.OnError(tid, err) // once per load, not once per waiting caller
			}
			return nil, err
		}
		p.mu.Lock()
		p.evictExpiredLocked()
		if p.shreds[tid] == gen {
			p.cache[tid] = cachedKey{dek: clone(dek), expires: p.cfg.Now().Add(p.cfg.TTL)}
		}
		p.mu.Unlock()
		return dek, nil
	})
	switch {
	case errors.Is(err, errShredded):
		return nil, ErrShredded
	case err != nil:
		return nil, ErrKeyUnavailable
	case p.shredCount(tid) != gen:
		// the tenant was shredded while its key was loading
		return nil, ErrShredded
	}
	return clone(v.([]byte)), nil
}

func (p *Provider) shredCount(tid tenant.ID) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.shreds[tid]
}

// Unlocked is true for every tenant: its key is created on first use and needs no unlocking.
func (p *Provider) Unlocked(ctx context.Context) bool {
	_, err := tenant.FromContext(ctx)
	return err == nil
}

// Forget drops the tenant's DEK from this instance's memory.
func (p *Provider) Forget(tid tenant.ID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.cache[tid]; ok {
		zero(c.dek)
		delete(p.cache, tid)
	}
}

// ForgetAll drops every tenant's DEK from this instance's memory.
func (p *Provider) ForgetAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for tid, c := range p.cache {
		zero(c.dek)
		delete(p.cache, tid)
	}
}

// Shred replaces the tenant's wrapped DEK with a tombstone, so its vault values can no longer be decrypted
// (crypto-shredding) and no instance creates a new DEK for it: Key then fails with ErrShredded (410). This instance
// forgets the DEK at once, including loads in flight. Other instances keep their copy until their TTL expires and
// can read and write the vault until then, so block the tenant's API access before shredding.
func (p *Provider) Shred(ctx context.Context, tid tenant.ID) error {
	p.bumpShreds(tid)
	if err := p.cfg.Store.Shred(ctx, tid); err != nil {
		return fmt.Errorf("vaultkms: shred the wrapped key of tenant %s: %w", tid, err)
	}
	p.bumpShreds(tid)
	return nil
}

// bumpShreds makes the tenant's cached DEK and every load in flight stale.
func (p *Provider) bumpShreds(tid tenant.ID) {
	p.mu.Lock()
	p.shreds[tid]++
	if c, ok := p.cache[tid]; ok {
		zero(c.dek)
		delete(p.cache, tid)
	}
	p.mu.Unlock()
	p.loads.Forget(string(tid))
}

// evictExpiredLocked drops every expired DEK, so idle tenants' keys don't stay in memory. p.mu must be held.
func (p *Provider) evictExpiredLocked() {
	now := p.cfg.Now()
	for tid, c := range p.cache {
		if !now.Before(c.expires) {
			zero(c.dek)
			delete(p.cache, tid)
		}
	}
}

// cached returns a copy of the tenant's DEK when it is in memory and not expired.
func (p *Provider) cached(tid tenant.ID) ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.cache[tid]
	if !ok {
		return nil, false
	}
	if !p.cfg.Now().Before(c.expires) {
		zero(c.dek)
		delete(p.cache, tid)
		return nil, false
	}
	return clone(c.dek), true
}

// load unwraps the tenant's stored DEK, or creates, wraps and stores one.
func (p *Provider) load(ctx context.Context, tid tenant.ID) ([]byte, error) {
	stored, found, err := p.cfg.Store.Get(ctx, tid)
	if err != nil {
		return nil, fmt.Errorf("vaultkms: read the wrapped key of tenant %s: %w", tid, err)
	}
	if found {
		return p.unwrap(ctx, tid, stored) // refuses a tombstone
	}

	dek := make([]byte, vault.KeySize)
	if _, err := io.ReadFull(p.cfg.Rand, dek); err != nil {
		return nil, fmt.Errorf("vaultkms: generate a key: %w", err)
	}
	name, err := p.cfg.KeyName(tid)
	if err != nil {
		return nil, fmt.Errorf("vaultkms: KMS key of tenant %s: %w", tid, err)
	}
	wrapped, err := p.wrap(ctx, tid, name, dek)
	if err != nil {
		return nil, err
	}
	winner, err := p.cfg.Store.CreateIfAbsent(ctx, tid, wrapped)
	if err != nil {
		return nil, fmt.Errorf("vaultkms: store the wrapped key of tenant %s: %w", tid, err)
	}
	if winner.equal(wrapped) {
		return dek, nil
	}
	// another instance stored its DEK first: use that one
	zero(dek)
	return p.unwrap(ctx, tid, winner)
}

// aad binds a wrapped DEK to its tenant: moved to another tenant's row, it no longer unwraps.
func aad(tid tenant.ID) []byte {
	return []byte("rport-vault-dek:v1:" + string(tid))
}

func (p *Provider) wrap(ctx context.Context, tid tenant.ID, name string, dek []byte) (WrappedKey, error) {
	ad := aad(tid)
	resp, err := p.cfg.KMS.Encrypt(ctx, &kmspb.EncryptRequest{
		Name:                              name,
		Plaintext:                         dek,
		PlaintextCrc32C:                   wrapperspb.Int64(crc(dek)),
		AdditionalAuthenticatedData:       ad,
		AdditionalAuthenticatedDataCrc32C: wrapperspb.Int64(crc(ad)),
	})
	if err != nil {
		return WrappedKey{}, fmt.Errorf("vaultkms: wrap the key of tenant %s: %w", tid, err)
	}
	// integrity checks recommended by Cloud KMS: the request arrived intact and so did the response
	if !strings.HasPrefix(resp.GetName(), name+"/cryptoKeyVersions/") {
		return WrappedKey{}, fmt.Errorf("vaultkms: wrap the key of tenant %s: KMS answered for key %q", tid, resp.GetName())
	}
	if !resp.GetVerifiedPlaintextCrc32C() || !resp.GetVerifiedAdditionalAuthenticatedDataCrc32C() {
		return WrappedKey{}, fmt.Errorf("vaultkms: wrap the key of tenant %s: the request was corrupted in transit", tid)
	}
	if resp.GetCiphertextCrc32C() == nil || resp.GetCiphertextCrc32C().GetValue() != crc(resp.GetCiphertext()) {
		return WrappedKey{}, fmt.Errorf("vaultkms: wrap the key of tenant %s: the response was corrupted in transit", tid)
	}
	return WrappedKey{KeyName: name, Ciphertext: resp.GetCiphertext()}, nil
}

func (p *Provider) unwrap(ctx context.Context, tid tenant.ID, w WrappedKey) ([]byte, error) {
	if w.Shredded {
		return nil, errShredded
	}
	ad := aad(tid)
	resp, err := p.cfg.KMS.Decrypt(ctx, &kmspb.DecryptRequest{
		Name:                              w.KeyName,
		Ciphertext:                        w.Ciphertext,
		CiphertextCrc32C:                  wrapperspb.Int64(crc(w.Ciphertext)),
		AdditionalAuthenticatedData:       ad,
		AdditionalAuthenticatedDataCrc32C: wrapperspb.Int64(crc(ad)),
	})
	if err != nil {
		return nil, fmt.Errorf("vaultkms: unwrap the key of tenant %s: %w", tid, err)
	}
	if resp.GetPlaintextCrc32C() == nil || resp.GetPlaintextCrc32C().GetValue() != crc(resp.GetPlaintext()) {
		return nil, fmt.Errorf("vaultkms: unwrap the key of tenant %s: the response was corrupted in transit", tid)
	}
	if len(resp.GetPlaintext()) != vault.KeySize {
		return nil, fmt.Errorf("vaultkms: unwrap the key of tenant %s: %d bytes, want %d", tid, len(resp.GetPlaintext()), vault.KeySize)
	}
	return resp.GetPlaintext(), nil
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func crc(b []byte) int64 {
	return int64(crc32.Checksum(b, castagnoli))
}

func clone(b []byte) []byte {
	return append([]byte(nil), b...)
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
