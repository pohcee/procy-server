package oidcfake

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/oidc"
)

// StoreContract runs the behavior every oidc.Store must have on s. addAccount creates a login (an account) that
// the email of a sign-in can match; ctx is the context the store is used with (the tenant of the accounts on
// Postgres).
func StoreContract(ctx context.Context, t *testing.T, s oidc.Store, addAccount func(t *testing.T, username string)) {
	now := time.Now()

	t.Run("logins are taken once", func(t *testing.T) {
		l := oidc.Login{StateHash: oidc.Hash("state-1"), Nonce: "n", Verifier: "v", BindingHash: oidc.Hash("b"), ReturnTo: "/x",
			ExpiresAt: now.Add(oidc.LoginTTL)}
		require.NoError(t, s.SaveLogin(ctx, l))
		got, err := s.TakeLogin(ctx, l.StateHash, now)
		require.NoError(t, err)
		assert.Equal(t, l.Nonce, got.Nonce)
		assert.Equal(t, l.Verifier, got.Verifier)
		assert.Equal(t, l.BindingHash, got.BindingHash)
		assert.Equal(t, "/x", got.ReturnTo)
		_, err = s.TakeLogin(ctx, l.StateHash, now)
		assert.ErrorIs(t, err, oidc.ErrNotFound, "replayed state")
		_, err = s.TakeLogin(ctx, oidc.Hash("never"), now)
		assert.ErrorIs(t, err, oidc.ErrNotFound)

		old := oidc.Login{StateHash: oidc.Hash("state-old"), Nonce: "n", Verifier: "v", BindingHash: "b", ExpiresAt: now.Add(time.Minute)}
		require.NoError(t, s.SaveLogin(ctx, old))
		_, err = s.TakeLogin(ctx, old.StateHash, now.Add(2*time.Minute))
		assert.ErrorIs(t, err, oidc.ErrNotFound, "expired")
	})

	t.Run("tickets are used once", func(t *testing.T) {
		tk := oidc.Ticket{CodeHash: oidc.Hash("code-1"), Username: "someone", BindingHash: "b", ExpiresAt: now.Add(oidc.TicketTTL)}
		require.NoError(t, s.SaveTicket(ctx, tk))
		got, err := s.GetTicket(ctx, tk.CodeHash, now)
		require.NoError(t, err)
		assert.Equal(t, "someone", got.Username)
		assert.Equal(t, "b", got.BindingHash)
		require.NoError(t, s.UseTicket(ctx, tk.CodeHash, now))
		assert.ErrorIs(t, s.UseTicket(ctx, tk.CodeHash, now), oidc.ErrExpired)
		_, err = s.GetTicket(ctx, tk.CodeHash, now)
		assert.ErrorIs(t, err, oidc.ErrExpired)
		_, err = s.GetTicket(ctx, oidc.Hash("never"), now)
		assert.ErrorIs(t, err, oidc.ErrNotFound)

		late := oidc.Ticket{CodeHash: oidc.Hash("code-late"), Username: "someone", BindingHash: "b", ExpiresAt: now.Add(time.Minute)}
		require.NoError(t, s.SaveTicket(ctx, late))
		_, err = s.GetTicket(ctx, late.CodeHash, now.Add(2*time.Minute))
		assert.ErrorIs(t, err, oidc.ErrExpired)
		assert.ErrorIs(t, s.UseTicket(ctx, late.CodeHash, now.Add(2*time.Minute)), oidc.ErrExpired)
	})

	t.Run("identities", func(t *testing.T) {
		addAccount(t, "carol@example.com")
		addAccount(t, "dave@example.com")
		iss := "https://issuer.example"

		_, err := s.Resolve(ctx, oidc.Identity{Issuer: iss, Subject: "x", Email: "nobody@example.com", EmailVerified: true, Linkable: true})
		assert.ErrorIs(t, err, oidc.ErrUnknownAccount, "no account is created")

		linked, err := s.Linked(ctx, []string{"carol@example.com", "dave@example.com"})
		require.NoError(t, err)
		assert.Empty(t, linked)

		// first sign-in: matched by email ignoring case, and pinned
		u, err := s.Resolve(ctx, oidc.Identity{Issuer: iss, Subject: "sub-carol", Email: "Carol@Example.COM", EmailVerified: true, Linkable: true})
		require.NoError(t, err)
		assert.Equal(t, "carol@example.com", u)
		linked, err = s.Linked(ctx, []string{"carol@example.com", "dave@example.com"})
		require.NoError(t, err)
		assert.Equal(t, map[string]bool{"carol@example.com": true}, linked)

		// the same Google account after its email changed: still carol
		u, err = s.Resolve(ctx, oidc.Identity{Issuer: iss, Subject: "sub-carol", Email: "carol.new@example.com", EmailVerified: true, Linkable: true})
		require.NoError(t, err)
		assert.Equal(t, "carol@example.com", u)

		// another Google account with carol's address can't take her over
		_, err = s.Resolve(ctx, oidc.Identity{Issuer: iss, Subject: "sub-impostor", Email: "carol@example.com", EmailVerified: true, Linkable: true})
		assert.ErrorIs(t, err, oidc.ErrMismatch)

		// an identity whose email the provider doesn't vouch for is not pinned, but a pinned one still signs in
		_, err = s.Resolve(ctx, oidc.Identity{Issuer: iss, Subject: "sub-dave", Email: "dave@example.com", EmailVerified: true})
		assert.ErrorIs(t, err, oidc.ErrNotLinkable)
		u, err = s.Resolve(ctx, oidc.Identity{Issuer: iss, Subject: "sub-carol", Email: "carol@example.com", EmailVerified: true})
		require.NoError(t, err)
		assert.Equal(t, "carol@example.com", u)

		// unlinking lets the next sign-in pin again
		require.NoError(t, s.Unlink(ctx, "carol@example.com"))
		u, err = s.Resolve(ctx, oidc.Identity{Issuer: iss, Subject: "sub-carol-2", Email: "carol@example.com", EmailVerified: true, Linkable: true})
		require.NoError(t, err)
		assert.Equal(t, "carol@example.com", u)

		// a pinned subject can't be pinned to a second account
		u, err = s.Resolve(ctx, oidc.Identity{Issuer: iss, Subject: "sub-carol-2", Email: "dave@example.com", EmailVerified: true, Linkable: true})
		require.NoError(t, err)
		assert.Equal(t, "carol@example.com", u, "the pinned subject names carol, not dave")
	})
}
