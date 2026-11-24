package vaultkms

import (
	kms "cloud.google.com/go/kms/apiv1"

	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/openrport/openrport/db/migration/vaults"
	"github.com/openrport/openrport/db/sqlite"
	errors2 "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/server/vault"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

var testLog = logger.NewLogger("vault", logger.LogOutput{File: os.Stdout}, logger.LogLevelInfo)

const testKeyName = "projects/p/locations/l/keyRings/r/cryptoKeys/vault"

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fixture struct {
	kms   *fakeKMS
	store *MemoryStore
	clock *clock
	p     *Provider
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{kms: &fakeKMS{}, store: NewMemoryStore(), clock: &clock{now: time.Unix(1_800_000_000, 0)}}
	f.p = f.provider(t)
	return f
}

// provider returns another provider (another server instance) on the same KMS and store.
func (f *fixture) provider(t *testing.T) *Provider {
	t.Helper()
	p, err := New(Config{
		KMS:     f.kms,
		Store:   f.store,
		KeyName: func(tenant.ID) (string, error) { return testKeyName, nil },
		TTL:     time.Minute,
		Now:     f.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func ctxFor(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

func mustKey(t *testing.T, p *Provider, id tenant.ID) []byte {
	t.Helper()
	k, err := p.Key(ctxFor(id))
	if err != nil {
		t.Fatalf("Key(%s): %v", id, err)
	}
	if len(k) != vault.KeySize {
		t.Fatalf("Key(%s) is %d bytes", id, len(k))
	}
	return k
}

func TestProviderSatisfiesVaultKeyProvider(t *testing.T) {
	var _ vault.KeyProvider = (*Provider)(nil)
}

func TestNewValidatesConfig(t *testing.T) {
	name := func(tenant.ID) (string, error) { return testKeyName, nil }
	for label, cfg := range map[string]Config{
		"no kms":      {Store: NewMemoryStore(), KeyName: name},
		"no store":    {KMS: &fakeKMS{}, KeyName: name},
		"no key name": {KMS: &fakeKMS{}, Store: NewMemoryStore()},
		"negative":    {KMS: &fakeKMS{}, Store: NewMemoryStore(), KeyName: name, TTL: -time.Second},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New accepted the config", label)
		}
	}
}

func TestKeyCreatesDEKOnFirstUse(t *testing.T) {
	f := newFixture(t)
	a := mustKey(t, f.p, "a")
	b := mustKey(t, f.p, "b")
	if bytes.Equal(a, b) {
		t.Fatal("tenants share a DEK")
	}
	if got := f.kms.encrypts.Load(); got != 2 {
		t.Fatalf("encrypts = %d, want 2 (one wrap per tenant)", got)
	}
	stored, found, _ := f.store.Get(context.Background(), "a")
	if !found || stored.KeyName != testKeyName || bytes.Contains(stored.Ciphertext, a) {
		t.Fatalf("stored wrapped key = %+v found=%v", stored, found)
	}
	if !f.p.Unlocked(ctxFor("a")) || f.p.Unlocked(context.Background()) {
		t.Fatal("Unlocked: want true with a tenant, false without")
	}
	if _, err := f.p.Key(context.Background()); !errors.Is(err, tenant.ErrMissing) {
		t.Fatalf("Key without tenant: %v", err)
	}
}

func TestKeyCachedWithinTTL(t *testing.T) {
	f := newFixture(t)
	first := mustKey(t, f.p, "a")
	f.clock.Add(59 * time.Second)
	if !bytes.Equal(first, mustKey(t, f.p, "a")) {
		t.Fatal("key changed within the TTL")
	}
	if f.kms.decrypts.Load() != 0 {
		t.Fatal("the creating instance should not need to unwrap its own DEK")
	}

	// another instance unwraps the stored DEK once and gets the same key
	other := f.provider(t)
	for range 3 {
		if !bytes.Equal(first, mustKey(t, other, "a")) {
			t.Fatal("other instance got a different DEK")
		}
	}
	if got := f.kms.decrypts.Load(); got != 1 {
		t.Fatalf("decrypts = %d, want 1", got)
	}
	if got := f.kms.encrypts.Load(); got != 1 {
		t.Fatalf("encrypts = %d, want 1", got)
	}

	// the caller's copy does not change the cache
	k := mustKey(t, other, "a")
	k[0] ^= 0xff
	if !bytes.Equal(first, mustKey(t, other, "a")) {
		t.Fatal("Key returned the cached slice")
	}
}

func TestKeyRefetchedAfterTTL(t *testing.T) {
	f := newFixture(t)
	first := mustKey(t, f.p, "a")
	f.clock.Add(time.Minute)
	if !bytes.Equal(first, mustKey(t, f.p, "a")) {
		t.Fatal("key changed after the TTL")
	}
	if got := f.kms.decrypts.Load(); got != 1 {
		t.Fatalf("decrypts after the TTL = %d, want 1", got)
	}
}

func TestKeyConcurrentFirstUseAgreesOnOneDEK(t *testing.T) {
	f := newFixture(t)
	instances := []*Provider{f.p, f.provider(t), f.provider(t), f.provider(t)}
	// every instance finds no DEK and wraps its own before any of them stores one
	var arrived sync.WaitGroup
	arrived.Add(len(instances))
	f.kms.beforeEncrypt = func() { arrived.Done(); arrived.Wait() }

	keys := make([][]byte, len(instances))
	var wg sync.WaitGroup
	for i, p := range instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, err := p.Key(ctxFor("a"))
			if err != nil {
				t.Error(err)
			}
			keys[i] = k
		}()
	}
	wg.Wait()
	for i := range keys {
		if !bytes.Equal(keys[0], keys[i]) {
			t.Fatalf("instance %d got another DEK", i)
		}
	}
	if got := f.kms.encrypts.Load(); got != int32(len(instances)) {
		t.Fatalf("encrypts = %d, want %d", got, len(instances))
	}
}

func TestKeyConcurrentCallsInOneInstanceCollapse(t *testing.T) {
	f := newFixture(t)
	mustKey(t, f.p, "a")
	other := f.provider(t)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); mustKey(t, other, "a") }()
	}
	wg.Wait()
	if got := f.kms.decrypts.Load(); got > 2 {
		t.Fatalf("decrypts = %d, concurrent loads should collapse", got)
	}
}

func TestWrappedDEKBoundToTenant(t *testing.T) {
	f := newFixture(t)
	mustKey(t, f.p, "a")
	wrappedA, _, _ := f.store.Get(context.Background(), "a")
	// a database edit moves a's wrapped DEK to b
	if _, err := f.store.CreateIfAbsent(context.Background(), "b", wrappedA); err != nil {
		t.Fatal(err)
	}
	if _, err := f.provider(t).Key(ctxFor("b")); err == nil {
		t.Fatal("a's wrapped DEK unwrapped for tenant b")
	}
}

func TestKMSErrorsPropagate(t *testing.T) {
	f := newFixture(t)
	var reported []error
	p, err := New(Config{
		KMS: f.kms, Store: f.store, Now: f.clock.Now,
		KeyName: func(tenant.ID) (string, error) { return testKeyName, nil },
		OnError: func(_ tenant.ID, err error) { reported = append(reported, err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.kms.err = errors.New("rpc error: code = PermissionDenied desc = projects/p/... denied")
	_, err = p.Key(ctxFor("a"))
	var apiErr errors2.APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("err = %#v, want a 503 APIError", err)
	}
	if apiErr.Err != nil || bytes.Contains([]byte(err.Error()), []byte("projects/")) {
		t.Fatalf("the KMS error reaches the API caller: %v", err)
	}
	if len(reported) != 1 || !errors.Is(reported[0], f.kms.err) {
		t.Fatalf("OnError got %v", reported)
	}
	if _, found, _ := f.store.Get(context.Background(), "a"); found {
		t.Fatal("a DEK was stored although wrapping failed")
	}

	// a failure is not cached
	f.kms.err = nil
	mustKey(t, p, "a")
}

func TestKeyNameErrorPropagates(t *testing.T) {
	f := newFixture(t)
	p, _ := New(Config{KMS: f.kms, Store: f.store, KeyName: func(tenant.ID) (string, error) { return "", errors.New("no key for region") }})
	if _, err := p.Key(ctxFor("a")); err == nil {
		t.Fatal("want an error")
	}
	if f.kms.encrypts.Load() != 0 {
		t.Fatal("KMS called without a key name")
	}
}

func TestCRCMismatchRejected(t *testing.T) {
	f := newFixture(t)
	f.kms.corruptEncryptCRC = true
	if _, err := f.p.Key(ctxFor("a")); err == nil {
		t.Fatal("corrupted wrap accepted")
	}
	f.kms.corruptEncryptCRC = false
	f.kms.unverified = true
	if _, err := f.p.Key(ctxFor("a")); err == nil {
		t.Fatal("unverified wrap accepted")
	}
	f.kms.unverified = false
	mustKey(t, f.p, "a")

	f.kms.corruptDecryptCRC = true
	if _, err := f.provider(t).Key(ctxFor("a")); err == nil {
		t.Fatal("corrupted unwrap accepted")
	}
}

func TestShredLeavesTombstone(t *testing.T) {
	f := newFixture(t)
	mustKey(t, f.p, "a")
	mustKey(t, f.p, "b")
	other := f.provider(t)
	mustKey(t, other, "a")

	if err := f.p.Shred(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	stored, found, _ := f.store.Get(context.Background(), "a")
	if !found || !stored.Shredded || len(stored.Ciphertext) != 0 {
		t.Fatalf("stored = %+v found=%v, want a tombstone", stored, found)
	}
	if _, found, _ := f.store.Get(context.Background(), "b"); !found {
		t.Fatal("b's DEK was shredded")
	}
	encrypts := f.kms.encrypts.Load()
	if _, err := f.p.Key(ctxFor("a")); !errors.Is(err, ErrShredded) {
		t.Fatalf("Key after Shred = %v, want ErrShredded", err)
	}
	if f.kms.encrypts.Load() != encrypts {
		t.Fatal("a new DEK was created for a shredded tenant")
	}

	// another instance keeps its copy until the TTL, then gets the tombstone too
	mustKey(t, other, "a")
	f.clock.Add(time.Minute)
	if _, err := other.Key(ctxFor("a")); !errors.Is(err, ErrShredded) {
		t.Fatalf("other instance after the TTL = %v, want ErrShredded", err)
	}
	if _, err := f.provider(t).Key(ctxFor("a")); !errors.Is(err, ErrShredded) {
		t.Fatalf("new instance = %v, want ErrShredded", err)
	}
	mustKey(t, other, "b")
}

func TestShredDuringLoadCachesNothing(t *testing.T) {
	f := newFixture(t)
	mustKey(t, f.p, "a")
	b := f.provider(t)

	inDecrypt, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.kms.beforeDecrypt = func() { once.Do(func() { close(inDecrypt); <-release }) }
	errc := make(chan error, 1)
	go func() { _, err := b.Key(ctxFor("a")); errc <- err }()
	<-inDecrypt
	if err := b.Shred(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-errc; !errors.Is(err, ErrShredded) {
		t.Fatalf("load in flight during Shred = %v, want ErrShredded", err)
	}
	if _, err := b.Key(ctxFor("a")); !errors.Is(err, ErrShredded) {
		t.Fatalf("after Shred = %v, want ErrShredded (nothing cached)", err)
	}
}

func TestOnErrorOncePerLoad(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	reports := 0
	block := make(chan struct{})
	p, _ := New(Config{
		KMS: f.kms, Store: f.store, Now: f.clock.Now,
		KeyName: func(tenant.ID) (string, error) { return testKeyName, nil },
		OnError: func(tenant.ID, error) { mu.Lock(); reports++; mu.Unlock() },
	})
	f.kms.err = errors.New("unavailable")
	f.kms.beforeEncrypt = func() { <-block }
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = p.Key(ctxFor("a")) }()
	}
	time.Sleep(50 * time.Millisecond) // let the callers join the load
	close(block)
	wg.Wait()
	if reports < 1 || reports >= 5 {
		t.Fatalf("OnError called %d times for 5 callers sharing loads", reports)
	}
}

func TestEncryptAnsweredForAnotherKeyRejected(t *testing.T) {
	f := newFixture(t)
	f.kms.wrongName = true
	if _, err := f.p.Key(ctxFor("a")); err == nil {
		t.Fatal("wrap by another key accepted")
	}
}

func TestExpiredKeysEvicted(t *testing.T) {
	f := newFixture(t)
	mustKey(t, f.p, "a")
	f.clock.Add(time.Minute)
	mustKey(t, f.p, "b")
	f.p.mu.Lock()
	_, stillThere := f.p.cache["a"]
	f.p.mu.Unlock()
	if stillThere {
		t.Fatal("an idle tenant's expired DEK stays in memory")
	}
}

// The provider in the OSS vault manager, on the vault's SQLite storage.
func TestShredMakesValuesUnreadable(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(t.TempDir(), "vault.sqlite.db")
	factory := vault.NewStatefulDbProviderFactory(func() (vault.DbProvider, error) { return openVaultDB(path) }, &vault.NotInitDbProvider{})
	m := vault.NewManagerWithKeyProvider(factory, &vault.Aes256PassManager{}, f.p, testLog)
	defer m.Close()
	user := testUser{}

	if m.UsesPassphrase() {
		t.Fatal("a KMS-backed manager must not use a passphrase")
	}
	id, err := m.Store(ctxFor("a"), 0, &vault.InputValue{ClientID: "c1", Key: "k", Value: "secret", Type: vault.SecretType}, user)
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := m.GetOne(ctxFor("a"), int(id.ID), user)
	if err != nil || !found || got.Value != "secret" {
		t.Fatalf("GetOne = %q %v %v", got.Value, found, err)
	}

	// another server instance reads it too
	m2 := vault.NewManagerWithKeyProvider(factory, &vault.Aes256PassManager{}, f.provider(t), testLog)
	got, _, err = m2.GetOne(ctxFor("a"), int(id.ID), user)
	if err != nil || got.Value != "secret" {
		t.Fatalf("second instance: %q %v", got.Value, err)
	}

	if err := f.p.Shred(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	_, _, err = m.GetOne(ctxFor("a"), int(id.ID), user)
	if !errors.Is(err, ErrShredded) {
		t.Fatalf("GetOne after crypto-shredding = %v, want ErrShredded", err)
	}
	if stored, _, _ := f.store.Get(context.Background(), "a"); !stored.Shredded {
		t.Fatal("reading a shredded tenant created a new DEK")
	}
}

type testUser struct{}

func (testUser) GetGroups() []string { return nil }
func (testUser) GetUsername() string { return "admin" }

func openVaultDB(path string) (vault.DbProvider, error) {
	db, err := sqlite.New(path, vaults.FS, vault.DataSourceOptions)
	if err != nil {
		return nil, err
	}
	return vault.NewSqliteProvider(db, testLog), nil
}

func TestCloudClientSatisfiesKMS(t *testing.T) {
	var _ KMS = (*kms.KeyManagementClient)(nil)
}
