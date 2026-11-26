package vaultkeys_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/repotest"
	"github.com/pohcee/procy-server/share/tenant"

	"github.com/pohcee/procy-server/server/storage/postgres"
	"github.com/pohcee/procy-server/server/storage/postgres/pgtest"
	"github.com/pohcee/procy-server/server/storage/postgres/vaultkeys"
	"github.com/pohcee/procy-server/server/vault/vaultkms"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

const keyName = "projects/p/locations/l/keyRings/r/cryptoKeys/k"

func wrapped(c string) vaultkms.WrappedKey {
	return vaultkms.WrappedKey{KeyName: keyName, Ciphertext: []byte(c)}
}

// storeCases are the behavior of a vaultkms.WrappedKeyStore. MemoryStore and Store both run them.
func storeCases(t *testing.T, newStore func(t *testing.T) vaultkms.WrappedKeyStore) {
	ctx := context.Background()
	a, b := repotest.TenantA, repotest.TenantB

	t.Run("GetMissing", func(t *testing.T) {
		s := newStore(t)
		k, found, err := s.Get(ctx, a)
		require.NoError(t, err)
		assert.False(t, found)
		assert.Equal(t, vaultkms.WrappedKey{}, k)
	})

	t.Run("CreateThenGet", func(t *testing.T) {
		s := newStore(t)
		stored, err := s.CreateIfAbsent(ctx, a, wrapped("dek-a"))
		require.NoError(t, err)
		assert.Equal(t, wrapped("dek-a"), stored)
		k, found, err := s.Get(ctx, a)
		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, wrapped("dek-a"), k)
	})

	t.Run("CreateKeepsExisting", func(t *testing.T) {
		s := newStore(t)
		_, err := s.CreateIfAbsent(ctx, a, wrapped("first"))
		require.NoError(t, err)
		stored, err := s.CreateIfAbsent(ctx, a, wrapped("second"))
		require.NoError(t, err)
		assert.Equal(t, wrapped("first"), stored)
		k, _, err := s.Get(ctx, a)
		require.NoError(t, err)
		assert.Equal(t, wrapped("first"), k)
	})

	t.Run("TenantsHaveOwnKeys", func(t *testing.T) {
		s := newStore(t)
		_, err := s.CreateIfAbsent(ctx, a, wrapped("dek-a"))
		require.NoError(t, err)
		_, found, err := s.Get(ctx, b)
		require.NoError(t, err)
		assert.False(t, found)
		stored, err := s.CreateIfAbsent(ctx, b, wrapped("dek-b"))
		require.NoError(t, err)
		assert.Equal(t, wrapped("dek-b"), stored)
		require.NoError(t, s.Shred(ctx, b))
		k, _, err := s.Get(ctx, a)
		require.NoError(t, err)
		assert.Equal(t, wrapped("dek-a"), k, "shredding B leaves A's key")
	})

	t.Run("ShredLeavesTombstone", func(t *testing.T) {
		s := newStore(t)
		_, err := s.CreateIfAbsent(ctx, a, wrapped("dek-a"))
		require.NoError(t, err)
		require.NoError(t, s.Shred(ctx, a))
		k, found, err := s.Get(ctx, a)
		require.NoError(t, err)
		assert.True(t, found)
		assert.True(t, k.Shredded)
		assert.Empty(t, k.Ciphertext)
		assert.Empty(t, k.KeyName)

		stored, err := s.CreateIfAbsent(ctx, a, wrapped("new"))
		require.NoError(t, err)
		assert.True(t, stored.Shredded, "a tombstone blocks a new key")
		assert.Empty(t, stored.Ciphertext)
		require.NoError(t, s.Shred(ctx, a), "shredding twice")
	})

	t.Run("ShredWithoutKey", func(t *testing.T) {
		s := newStore(t)
		require.NoError(t, s.Shred(ctx, a))
		k, found, err := s.Get(ctx, a)
		require.NoError(t, err)
		assert.True(t, found)
		assert.True(t, k.Shredded)
		stored, err := s.CreateIfAbsent(ctx, a, wrapped("new"))
		require.NoError(t, err)
		assert.True(t, stored.Shredded)
	})

	t.Run("ReturnsCopies", func(t *testing.T) {
		s := newStore(t)
		in := wrapped("dek-a")
		stored, err := s.CreateIfAbsent(ctx, a, in)
		require.NoError(t, err)
		in.Ciphertext[0] = 'X'
		stored.Ciphertext[1] = 'X'
		k, _, err := s.Get(ctx, a)
		require.NoError(t, err)
		assert.Equal(t, wrapped("dek-a"), k)
	})

	t.Run("ConcurrentCreateAgreesOnWinner", func(t *testing.T) {
		s := newStore(t)
		const n = 8
		got := make([]vaultkms.WrappedKey, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				k, err := s.CreateIfAbsent(ctx, a, wrapped(fmt.Sprintf("%c", 'a'+i)))
				assert.NoError(t, err)
				got[i] = k
			}()
		}
		close(start)
		wg.Wait()
		winner, found, err := s.Get(ctx, a)
		require.NoError(t, err)
		require.True(t, found)
		for i := range got {
			assert.Equal(t, winner, got[i], "caller %d", i)
		}
	})
}

func TestMemoryStore(t *testing.T) {
	storeCases(t, func(*testing.T) vaultkms.WrappedKeyStore { return vaultkms.NewMemoryStore() })
}

func TestStore(t *testing.T) {
	storeCases(t, func(t *testing.T) vaultkms.WrappedKeyStore {
		d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
		return vaultkeys.New(d.App)
	})
}

func TestRefusesEmptyTenant(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	s := vaultkeys.New(d.App)
	ctx := repotest.Ctx(repotest.TenantA) // the tenant argument counts, not the context's
	_, _, err := s.Get(ctx, "")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = s.CreateIfAbsent(ctx, "", wrapped("x"))
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.ErrorIs(t, s.Shred(ctx, ""), tenant.ErrMissing)
	var n int
	require.NoError(t, d.Owner.Get(&n, "SELECT count(*) FROM vault_wrapped_keys"))
	assert.Zero(t, n)
}

func TestConcurrentCreateAcrossConnections(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	// two server instances: separate pools on the same database
	other, err := postgres.Open(context.Background(), d.AppURL)
	require.NoError(t, err)
	t.Cleanup(func() { other.Close() })
	stores := []*vaultkeys.Store{vaultkeys.New(d.App), vaultkeys.New(other)}

	// hold instance 0's insert open until instance 1 has blocked on the primary key
	ctx := context.Background()
	tx, err := d.App.BeginTxx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.Exec("SELECT set_config($1, $2, true)", postgres.TenantSetting, "a")
	require.NoError(t, err)
	_, err = tx.Exec("INSERT INTO vault_wrapped_keys (tenant_id, key_name, ciphertext) VALUES ('a', $1, 'winner')", keyName)
	require.NoError(t, err)

	done := make(chan vaultkms.WrappedKey)
	go func() {
		k, err := stores[1].CreateIfAbsent(ctx, repotest.TenantA, wrapped("loser"))
		assert.NoError(t, err)
		done <- k
	}()
	require.Eventually(t, func() bool {
		var waiting int
		_ = d.Owner.Get(&waiting, "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database()")
		return waiting > 0
	}, 10*time.Second, 20*time.Millisecond)
	require.NoError(t, tx.Commit())
	assert.Equal(t, wrapped("winner"), <-done)
	k, _, err := stores[0].Get(ctx, repotest.TenantA)
	require.NoError(t, err)
	assert.Equal(t, wrapped("winner"), k)
}

func seed(t *testing.T, d *pgtest.DB) {
	t.Helper()
	s := vaultkeys.New(d.App)
	for _, id := range []tenant.ID{repotest.TenantA, repotest.TenantB} {
		_, err := s.CreateIfAbsent(context.Background(), id, wrapped(string(id)))
		require.NoError(t, err)
	}
}

func TestRLSBackstop(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	seed(t, d)
	var tenants []string
	require.NoError(t, postgres.InTenant(repotest.Ctx(repotest.TenantA), d.App, func(tx *sqlx.Tx) error {
		return tx.Select(&tenants, "SELECT tenant_id FROM vault_wrapped_keys")
	}))
	assert.Equal(t, []string{"a"}, tenants)
	var n int
	require.NoError(t, d.App.Get(&n, "SELECT count(*) FROM vault_wrapped_keys"))
	assert.Zero(t, n)
}

func TestTenantDeleteCascades(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	seed(t, d)
	_, err := d.Owner.Exec("DELETE FROM tenants WHERE id = 'a'")
	require.NoError(t, err)
	var tenants []string
	require.NoError(t, d.Owner.Select(&tenants, "SELECT tenant_id FROM vault_wrapped_keys"))
	assert.Equal(t, []string{"b"}, tenants)
}

func TestShredSetsUpdatedAt(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	s := vaultkeys.New(d.App)
	_, err := s.CreateIfAbsent(context.Background(), repotest.TenantA, wrapped("k"))
	require.NoError(t, err)
	_, err = d.Owner.Exec("UPDATE vault_wrapped_keys SET created_at = now() - interval '1 day', updated_at = now() - interval '1 day'")
	require.NoError(t, err)
	require.NoError(t, s.Shred(context.Background(), repotest.TenantA))
	var row struct {
		Ciphertext []byte `db:"ciphertext"`
		Fresh      bool   `db:"fresh"`
		Old        bool   `db:"old"`
	}
	require.NoError(t, d.Owner.Get(&row, `SELECT ciphertext, updated_at > now() - interval '1 hour' AS fresh,
		created_at < now() - interval '1 hour' AS old FROM vault_wrapped_keys`))
	assert.Nil(t, row.Ciphertext, "the wrapped key is gone")
	assert.True(t, row.Fresh)
	assert.True(t, row.Old, "created_at is kept")
}
