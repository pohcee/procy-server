package clientsauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Client credentials of a cluster (Postgres storage) are stored as argon2id hashes; standalone providers keep the
// passwords as the admin wrote them and compare them as they are.

// argon2id parameters of new hashes (the OWASP minimum: 19 MiB, 2 passes, 1 lane). A hash records its own
// parameters, so they can be raised later without invalidating stored hashes.
const (
	hashMemoryKiB = 19 * 1024
	hashTime      = 2
	hashThreads   = 1
	hashSaltLen   = 16
	hashKeyLen    = 32
	hashPrefix    = "$argon2id$"
	// A stored hash's parameters must lie near the defaults: a crafted hash can neither exhaust a device process's
	// memory or CPU nor be trivially weak.
	minHashMemoryKiB = 8 * 1024
	maxHashMemoryKiB = 64 * 1024
	maxHashTime      = 4
	maxHashThreads   = 4
)

// verifySlots bounds the argon2 computations running at once (each takes hashMemoryKiB), so a burst of logins
// can't exhaust memory.
var verifySlots = make(chan struct{}, max(2, runtime.NumCPU()))

var b64 = base64.RawStdEncoding

// HashPassword returns the argon2id hash of password, in the PHC string form
// $argon2id$v=19$m=<KiB>,t=<passes>,p=<lanes>$<salt>$<key> (unpadded base64).
func HashPassword(password string) (string, error) {
	salt := make([]byte, hashSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("client auth: salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, hashTime, hashMemoryKiB, hashThreads, hashKeyLen)
	return fmt.Sprintf("%sv=%d$m=%d,t=%d,p=%d$%s$%s", hashPrefix, argon2.Version, hashMemoryKiB, hashTime, hashThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// IsHash reports whether stored has the form of a hash made by HashPassword.
func IsHash(stored string) bool {
	_, err := parseHash(stored)
	return err == nil
}

// VerifyHash reports whether password matches hash, an argon2id hash made by HashPassword. Anything else (a
// plaintext password, a malformed hash) never matches.
func VerifyHash(hash string, password []byte) bool {
	h, err := parseHash(hash)
	if err != nil {
		return false
	}
	verifySlots <- struct{}{}
	key := argon2.IDKey(password, h.salt, h.time, h.memory, h.threads, uint32(len(h.key))) //nolint:gosec // G115: key length checked in parseHash
	<-verifySlots
	return subtle.ConstantTimeCompare(key, h.key) == 1
}

type parsedHash struct {
	memory, time uint32
	threads      uint8
	salt, key    []byte
}

var errBadHash = errors.New("not an argon2id hash")

func parseHash(s string) (*parsedHash, error) {
	if !strings.HasPrefix(s, hashPrefix) {
		return nil, errBadHash
	}
	parts := strings.Split(strings.TrimPrefix(s, hashPrefix), "$")
	if len(parts) != 4 {
		return nil, errBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[0], "v=%d", &version); err != nil || version != argon2.Version {
		return nil, errBadHash
	}
	h := &parsedHash{}
	if _, err := fmt.Sscanf(parts[1], "m=%d,t=%d,p=%d", &h.memory, &h.time, &h.threads); err != nil {
		return nil, errBadHash
	}
	if h.memory < minHashMemoryKiB || h.memory > maxHashMemoryKiB || h.time == 0 || h.time > maxHashTime ||
		h.threads == 0 || h.threads > maxHashThreads {
		return nil, errBadHash
	}
	var err error
	if h.salt, err = b64.DecodeString(parts[2]); err != nil || len(h.salt) < 8 {
		return nil, errBadHash
	}
	if h.key, err = b64.DecodeString(parts[3]); err != nil || len(h.key) < 16 || len(h.key) > 64 {
		return nil, errBadHash
	}
	return h, nil
}

// GeneratePassword returns a random password (24 characters, URL-safe base64 of 18 random bytes).
func GeneratePassword() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("client auth: generate password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Verifier is a Provider that checks passwords itself, because it stores only hashes: its Get and GetFiltered
// return the credentials with an empty Password.
type Verifier interface {
	// Verify reports whether password is the one of the credentials id in the context's tenant (false when there
	// are none).
	Verify(ctx context.Context, id string, password []byte) (bool, error)
}

// StoresHashes reports whether p keeps only password hashes (it implements Verifier): the API then returns a new
// credential's password once, in the response that created it.
func StoresHashes(p Provider) bool {
	_, ok := p.(Verifier)
	return ok
}

// Verify reports whether password is the one of the client credentials id in p, in the context's tenant. A
// Verifier checks it against its hash; any other provider (standalone: file, single, [database]) compares the
// stored password as it is, in constant time, as the client listener always did.
func Verify(ctx context.Context, p Provider, id string, password []byte) (bool, error) {
	if v, ok := p.(Verifier); ok {
		return v.Verify(ctx, id, password)
	}
	c, err := p.Get(ctx, id)
	if err != nil {
		return false, err
	}
	if c == nil {
		return false, nil
	}
	return subtle.ConstantTimeCompare([]byte(c.Password), password) == 1, nil
}
