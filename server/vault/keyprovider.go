package vault

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"sync"

	"github.com/openrport/openrport/share/enc"
	"github.com/openrport/openrport/share/tenant"
)

// KeySize is the length in bytes of the key a KeyProvider returns (AES-256).
const KeySize = 32

// KeyProvider gives the key that encrypts and decrypts the vault values of the tenant in ctx (#34).
//
// The default, PassphraseKeyProvider, derives the key from the passphrase an admin gives through the vault API, and
// the vault is locked until then. Any other provider is a managed provider: the server gets the key without a
// passphrase (for example from an external key service), so the vault needs no initialisation, cannot be locked or
// unlocked through the API, and the server reports the vault_passphrase capability as false.
type KeyProvider interface {
	// Key returns the KeySize-byte key of ctx's tenant. Its error is returned to the API caller as is, so an
	// errors.APIError keeps its HTTP status.
	Key(ctx context.Context) ([]byte, error)
	// Unlocked reports whether the vault of ctx's tenant can be used now.
	Unlocked(ctx context.Context) bool
}

// PassphraseKeyProvider is the default KeyProvider. It keeps the passphrase of each unlocked tenant's vault in
// memory, so a restart locks every vault, and derives the key from it as SHA-256 of the passphrase.
type PassphraseKeyProvider struct {
	lock   sync.RWMutex
	passes map[tenant.ID]string
}

var _ KeyProvider = (*PassphraseKeyProvider)(nil)

func NewPassphraseKeyProvider() *PassphraseKeyProvider {
	return &PassphraseKeyProvider{passes: map[tenant.ID]string{}}
}

func (p *PassphraseKeyProvider) Key(ctx context.Context) ([]byte, error) {
	tid, err := vaultTenant(ctx)
	if err != nil {
		return nil, err
	}
	pass := p.passOf(tid)
	if pass == "" {
		return nil, errVaultLocked
	}
	return passphraseKey(pass), nil
}

func (p *PassphraseKeyProvider) Unlocked(ctx context.Context) bool {
	tid, err := vaultTenant(ctx)
	if err != nil {
		return false
	}
	return p.passOf(tid) != ""
}

// passOf returns the passphrase of the tenant's vault, "" when it is locked.
func (p *PassphraseKeyProvider) passOf(tid tenant.ID) string {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.passes[tid]
}

// setPass unlocks the tenant's vault with pass, or locks it when pass is "".
func (p *PassphraseKeyProvider) setPass(tid tenant.ID, pass string) {
	p.lock.Lock()
	defer p.lock.Unlock()
	if pass == "" {
		delete(p.passes, tid)
		return
	}
	p.passes[tid] = pass
}

// passphraseKey is the key the vault has always derived from a passphrase (see enc.Aes256EncryptByPassToBase64String).
func passphraseKey(pass string) []byte {
	sum := sha256.Sum256([]byte(pass))
	return sum[:]
}

// encryptValue encrypts a vault value with key: AES-256-GCM, the nonce prefixed, base64 encoded. With a passphrase
// key this is byte for byte what enc.Aes256EncryptByPassToBase64String produces.
func encryptValue(plain, key []byte) (string, error) {
	if len(key) != KeySize {
		return "", fmt.Errorf("vault: the key provider returned a %d-byte key, %d bytes expected", len(key), KeySize)
	}
	encrypted, err := enc.Aes256Encrypt(plain, key)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(encrypted), nil
}

// decryptValue reverses encryptValue.
func decryptValue(encoded string, key []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("vault: the key provider returned a %d-byte key, %d bytes expected", len(key), KeySize)
	}
	encrypted, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	return enc.AesDecrypt(encrypted, key)
}
