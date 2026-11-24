package repotest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api/session"
	"github.com/openrport/openrport/share/tenant"
)

// APISessions checks a session.StorageProvider (API sessions).
func APISessions(t *testing.T, newRepo Factory[session.StorageProvider]) {
	a, b := Ctx(TenantA), Ctx(TenantB)

	t.Run("RequiresTenant", func(t *testing.T) {
		p := newRepo(t)
		ctx := context.Background()
		_, _, err := p.Get(ctx, 1)
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = p.GetAll(ctx)
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = p.Save(ctx, liveAPISession("u"))
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = p.Save(ctx, session.APISession{SessionID: 1, Username: "u"})
		assert.ErrorIs(t, err, tenant.ErrMissing)
		assert.ErrorIs(t, p.Delete(ctx, 1), tenant.ErrMissing)
		assert.ErrorIs(t, p.DeleteExpired(ctx), tenant.ErrMissing)
		assert.ErrorIs(t, p.DeleteAllByUser(ctx, "u"), tenant.ErrMissing)
		assert.ErrorIs(t, p.DeleteByID(ctx, "u", 1), tenant.ErrMissing)
	})

	t.Run("SaveAndGet", func(t *testing.T) {
		p := newRepo(t)
		s := liveAPISession("u")
		id, err := p.Save(a, s)
		require.NoError(t, err)
		assert.Positive(t, id)
		id2, err := p.Save(a, liveAPISession("u"))
		require.NoError(t, err)
		assert.NotEqual(t, id, id2, "each new session gets its own id")

		found, got, err := p.Get(a, id)
		require.NoError(t, err)
		require.True(t, found)
		s.SessionID = id
		s.TenantID = TenantA
		assertSameSession(t, s, got)

		found, _, err = p.Get(a, id2+1000)
		require.NoError(t, err)
		assert.False(t, found)
	})

	t.Run("SaveUpdatesExisting", func(t *testing.T) {
		p := newRepo(t)
		id, err := p.Save(a, expiredAPISession("u"))
		require.NoError(t, err)
		now := msNow()
		upd := session.APISession{
			SessionID: id, Username: "u2", ExpiresAt: now.Add(2 * time.Hour), LastAccessAt: now.Add(time.Minute),
			UserAgent: "ua2", IPAddress: "5.6.7.8",
		}
		gotID, err := p.Save(a, upd)
		require.NoError(t, err)
		assert.Equal(t, id, gotID)
		_, got, err := p.Get(a, id)
		require.NoError(t, err)
		upd.TenantID = TenantA
		assertSameSession(t, upd, got)
	})

	t.Run("GetAllOnlyUnexpired", func(t *testing.T) {
		p := newRepo(t)
		live1, err := p.Save(a, liveAPISession("u"))
		require.NoError(t, err)
		live2, err := p.Save(a, liveAPISession("v"))
		require.NoError(t, err)
		expired, err := p.Save(a, expiredAPISession("u"))
		require.NoError(t, err)

		all, err := p.GetAll(a)
		require.NoError(t, err)
		assert.ElementsMatch(t, []int64{live1, live2}, sessionIDs(all))
		for _, s := range all {
			assert.Equal(t, TenantA, s.TenantID)
		}
		found, _, err := p.Get(a, expired)
		require.NoError(t, err)
		assert.True(t, found, "expired sessions stay until DeleteExpired")
	})

	t.Run("Deletes", func(t *testing.T) {
		p := newRepo(t)
		s1, err := p.Save(a, liveAPISession("u"))
		require.NoError(t, err)
		s2, err := p.Save(a, liveAPISession("u"))
		require.NoError(t, err)
		s3, err := p.Save(a, liveAPISession("u"))
		require.NoError(t, err)
		v1, err := p.Save(a, liveAPISession("v"))
		require.NoError(t, err)
		exp, err := p.Save(a, expiredAPISession("v"))
		require.NoError(t, err)

		require.NoError(t, p.Delete(a, s1))
		require.NoError(t, p.DeleteByID(a, "v", s2), "the username must match")
		requireSessionFound(a, t, p, s2, true)
		require.NoError(t, p.DeleteByID(a, "u", s2))
		requireSessionFound(a, t, p, s2, false)
		requireSessionFound(a, t, p, s1, false)

		require.NoError(t, p.DeleteExpired(a))
		requireSessionFound(a, t, p, exp, false)
		requireSessionFound(a, t, p, v1, true)

		require.NoError(t, p.DeleteAllByUser(a, "u"))
		requireSessionFound(a, t, p, s3, false)
		requireSessionFound(a, t, p, v1, true)

		assert.NoError(t, p.Delete(a, 999999), "deleting a missing session is not an error")
		assert.NoError(t, p.DeleteAllByUser(a, "nobody"))
	})

	t.Run("IsolatesTenants", func(t *testing.T) {
		p := newRepo(t)
		aID, err := p.Save(a, liveAPISession("u"))
		require.NoError(t, err)
		bID, err := p.Save(b, liveAPISession("u"))
		require.NoError(t, err)
		assert.NotEqual(t, aID, bID, "session ids are unique across tenants")
		aExpired, err := p.Save(a, expiredAPISession("u"))
		require.NoError(t, err)
		bExpired, err := p.Save(b, expiredAPISession("u"))
		require.NoError(t, err)

		found, _, err := p.Get(b, aID)
		require.NoError(t, err)
		assert.False(t, found)
		found, got, err := p.Get(a, aID)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, TenantA, got.TenantID)

		all, err := p.GetAll(a)
		require.NoError(t, err)
		assert.Equal(t, []int64{aID}, sessionIDs(all))

		// b can't update a's session
		_, err = p.Save(b, session.APISession{SessionID: aID, Username: "u", ExpiresAt: time.Now().Add(time.Hour), UserAgent: "hijack"})
		require.NoError(t, err)
		_, got, err = p.Get(a, aID)
		require.NoError(t, err)
		assert.Equal(t, "ua", got.UserAgent)

		// deletes in b don't touch a
		require.NoError(t, p.Delete(b, aID))
		require.NoError(t, p.DeleteByID(b, "u", aID))
		require.NoError(t, p.DeleteExpired(b))
		requireSessionFound(a, t, p, aExpired, true)
		requireSessionFound(b, t, p, bExpired, false)

		require.NoError(t, p.DeleteAllByUser(b, "u"))
		requireSessionFound(a, t, p, aID, true)
		requireSessionFound(b, t, p, bID, false)
	})

	t.Run("IgnoresTenantOnStruct", func(t *testing.T) {
		p := newRepo(t)
		s := liveAPISession("u")
		s.TenantID = TenantB
		id, err := p.Save(a, s)
		require.NoError(t, err)
		requireSessionFound(b, t, p, id, false)
		found, got, err := p.Get(a, id)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, TenantA, got.TenantID)
	})
}

func msNow() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

func liveAPISession(username string) session.APISession {
	now := msNow()
	return session.APISession{Username: username, ExpiresAt: now.Add(time.Hour), LastAccessAt: now, UserAgent: "ua", IPAddress: "1.2.3.4"}
}

func expiredAPISession(username string) session.APISession {
	s := liveAPISession(username)
	s.ExpiresAt = s.LastAccessAt.Add(-time.Minute)
	return s
}

func assertSameSession(t *testing.T, want, got session.APISession) {
	t.Helper()
	assert.Equal(t, want.TenantID, got.TenantID)
	assert.Equal(t, want.SessionID, got.SessionID)
	assert.Equal(t, want.Username, got.Username)
	assert.Equal(t, want.UserAgent, got.UserAgent)
	assert.Equal(t, want.IPAddress, got.IPAddress)
	assertSameTime(t, want.ExpiresAt, &got.ExpiresAt)
	assertSameTime(t, want.LastAccessAt, &got.LastAccessAt)
}

func requireSessionFound(ctx context.Context, t *testing.T, p session.StorageProvider, id int64, want bool) {
	t.Helper()
	found, _, err := p.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, want, found, "session %d found", id)
}

func sessionIDs(ss []session.APISession) []int64 {
	out := make([]int64, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.SessionID)
	}
	return out
}
