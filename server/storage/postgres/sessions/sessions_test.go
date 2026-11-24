package sessions

import (
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api/session"
	"github.com/openrport/openrport/server/repotest"

	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestContract(t *testing.T) {
	repotest.APISessions(t, func(t *testing.T) session.StorageProvider {
		return New(pgtest.New(t, repotest.TenantA, repotest.TenantB).App)
	})
}

func live(username string) session.APISession {
	now := time.Now()
	return session.APISession{Username: username, ExpiresAt: now.Add(time.Hour), LastAccessAt: now, UserAgent: "ua", IPAddress: "1.2.3.4"}
}

func TestRLSBackstop(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	s := New(d.App)
	a, b := repotest.Ctx(repotest.TenantA), repotest.Ctx(repotest.TenantB)
	_, err := s.Save(a, live("u"))
	require.NoError(t, err)
	_, err = s.Save(b, live("u"))
	require.NoError(t, err)

	var tenants []string
	require.NoError(t, postgres.InTenant(a, d.App, func(tx *sqlx.Tx) error {
		return tx.Select(&tenants, "SELECT tenant_id FROM api_sessions")
	}))
	assert.Equal(t, []string{"a"}, tenants)
	var n int
	require.NoError(t, d.App.Get(&n, "SELECT count(*) FROM api_sessions"))
	assert.Zero(t, n)
}

func TestDeletingTenantCascades(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	s := New(d.App)
	_, err := s.Save(repotest.Ctx(repotest.TenantA), live("u"))
	require.NoError(t, err)
	bID, err := s.Save(repotest.Ctx(repotest.TenantB), live("u"))
	require.NoError(t, err)
	_, err = d.Owner.Exec("DELETE FROM tenants WHERE id = 'a'")
	require.NoError(t, err)
	var ids []int64
	require.NoError(t, d.Owner.Select(&ids, "SELECT session_id FROM api_sessions"))
	assert.Equal(t, []int64{bID}, ids)
}

func TestNullTextColumnsReadEmpty(t *testing.T) {
	// rows copied from SQLite may have no user agent or IP address
	d := pgtest.New(t, repotest.TenantA)
	var id int64
	require.NoError(t, d.Owner.Get(&id, `INSERT INTO api_sessions (tenant_id, expires_at, username, last_access_at)
		VALUES ('a', now() + interval '1 hour', 'u', now()) RETURNING session_id`))
	found, got, err := New(d.App).Get(repotest.Ctx(repotest.TenantA), id)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "", got.UserAgent)
	assert.Equal(t, "", got.IPAddress)
}
