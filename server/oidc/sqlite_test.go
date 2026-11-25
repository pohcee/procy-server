package oidc_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apisessions "github.com/openrport/openrport/db/migration/api_sessions"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/oidc"
	"github.com/openrport/openrport/server/oidc/oidcfake"
)

func TestSQLiteStore(t *testing.T) {
	db, err := sqlite.New(filepath.Join(t.TempDir(), "api_sessions.db"), apisessions.FS, sqlite.DataSourceOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	s := oidc.NewSQLiteStore(db)
	var mu sync.Mutex
	var logins []string
	s.SetLogins(func(context.Context) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), logins...), nil
	})
	oidcfake.StoreContract(context.Background(), t, s, func(_ *testing.T, u string) {
		mu.Lock()
		defer mu.Unlock()
		logins = append(logins, u)
	})

	// two logins differing only in case are ambiguous
	logins = append(logins, "Eve@example.com", "eve@example.com")
	_, err = s.Resolve(context.Background(), oidc.Identity{Issuer: "i", Subject: "eve", Email: "eve@example.com", EmailVerified: true})
	assert.ErrorIs(t, err, oidc.ErrAmbiguous)
}
