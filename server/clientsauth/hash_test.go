package clientsauth

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/share/tenant"
)

func TestHashRoundTrip(t *testing.T) {
	h, err := HashPassword("s3cret")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$"), h)
	assert.True(t, IsHash(h))
	assert.True(t, VerifyHash(h, []byte("s3cret")))
	assert.False(t, VerifyHash(h, []byte("s3cre")))
	assert.False(t, VerifyHash(h, nil))

	h2, err := HashPassword("s3cret")
	require.NoError(t, err)
	assert.NotEqual(t, h, h2, "salted")
}

func TestVerifyHashRefusesNonHashes(t *testing.T) {
	for _, s := range []string{"", "s3cret", "$argon2id$", "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$", "$2a$10$abcdefghijklmnopqrstuv",
		"$argon2id$v=19$m=99999999,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",   // too much memory
		"$argon2id$v=19$m=1048576,t=16,p=255$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA", // a DoS hash
		"$argon2id$v=19$m=1,t=1,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",          // trivially weak
		"$argon2i$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA"} {
		assert.False(t, IsHash(s), s)
		assert.False(t, VerifyHash(s, []byte(s)), s)
	}
}

type hashingProvider struct {
	*SingleProvider
	hash string
}

func (p hashingProvider) Verify(_ context.Context, id string, pw []byte) (bool, error) {
	return id == "c" && VerifyHash(p.hash, pw), nil
}

func TestVerify(t *testing.T) {
	ctx := tenant.WithID(context.Background(), tenant.Default)
	plain := NewSingleProvider("c", "pw")
	ok, err := Verify(ctx, plain, "c", []byte("pw"))
	require.NoError(t, err)
	assert.True(t, ok)
	ok, _ = Verify(ctx, plain, "c", []byte("nope"))
	assert.False(t, ok)
	ok, _ = Verify(ctx, plain, "other", []byte("pw"))
	assert.False(t, ok)
	assert.False(t, StoresHashes(plain))

	// a standalone password that looks like a hash is still compared as it is
	looksHashed := "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA"
	ok, _ = Verify(ctx, NewSingleProvider("c", looksHashed), "c", []byte(looksHashed))
	assert.True(t, ok)

	h, err := HashPassword("pw")
	require.NoError(t, err)
	hp := hashingProvider{SingleProvider: plain, hash: h}
	assert.True(t, StoresHashes(hp))
	ok, _ = Verify(ctx, hp, "c", []byte("pw"))
	assert.True(t, ok)
}
