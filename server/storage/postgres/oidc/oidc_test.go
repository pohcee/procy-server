package oidc_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/oidc"
	"github.com/openrport/openrport/server/oidc/oidcfake"
	pgoidc "github.com/openrport/openrport/server/storage/postgres/oidc"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	pgusers "github.com/openrport/openrport/server/storage/postgres/users"
	"github.com/openrport/openrport/share/tenant"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

const tenantA, tenantB tenant.ID = "a", "b"

func addUser(t *testing.T, d *pgtest.DB, tid tenant.ID, username string) {
	t.Helper()
	err := pgusers.New(d.App).Add(tenant.WithID(context.Background(), tid), &users.User{Username: username, Password: "x", Groups: []string{users.Administrators}})
	require.NoError(t, err)
}

func TestStoreContract(t *testing.T) {
	d := pgtest.New(t, tenantA)
	ctx := tenant.WithID(context.Background(), tenantA)
	oidcfake.StoreContract(ctx, t, pgoidc.New(d.App), func(t *testing.T, u string) { addUser(t, d, tenantA, u) })
}

func TestResolveVerifiesEmailAndUnpinsOnRename(t *testing.T) {
	d := pgtest.New(t, tenantA)
	ctx := tenant.WithID(context.Background(), tenantA)
	s := pgoidc.New(d.App)
	addUser(t, d, tenantA, "erin@example.com")

	u, err := s.Resolve(ctx, oidc.Identity{Issuer: "i", Subject: "erin", Email: "ERIN@example.com", EmailVerified: true})
	require.NoError(t, err)
	assert.Equal(t, "erin@example.com", u)
	var verified bool
	require.NoError(t, d.Owner.Get(&verified, "SELECT email_verified_at IS NOT NULL FROM accounts WHERE username = 'erin@example.com'"))
	assert.True(t, verified, "a Google sign-in proves the mailbox")

	// the application role can't write identities directly
	_, err = d.App.Exec("INSERT INTO account_identities (issuer, subject, username) VALUES ('i', 'other', 'erin@example.com')")
	assert.Error(t, err)

	// renaming the account drops the pin
	require.NoError(t, pgusers.New(d.App).Update(ctx, &users.User{Username: "erin2@example.com"}, "erin@example.com"))
	linked, err := s.Linked(ctx, []string{"erin2@example.com"})
	require.NoError(t, err)
	assert.Empty(t, linked)
}

func TestAmbiguousEmail(t *testing.T) {
	d := pgtest.New(t, tenantA)
	ctx := tenant.WithID(context.Background(), tenantA)
	addUser(t, d, tenantA, "Frank@example.com")
	addUser(t, d, tenantA, "frank@example.com")
	_, err := pgoidc.New(d.App).Resolve(ctx, oidc.Identity{Issuer: "i", Subject: "f", Email: "frank@example.com", EmailVerified: true})
	assert.ErrorIs(t, err, oidc.ErrAmbiguous)
}

func TestPolicies(t *testing.T) {
	d := pgtest.New(t, tenantA, tenantB)
	ctxA := tenant.WithID(context.Background(), tenantA)
	ctxB := tenant.WithID(context.Background(), tenantB)
	s := pgoidc.New(d.App)
	addUser(t, d, tenantA, "gina@example.com")

	p, err := s.Policy(ctxA, "gina@example.com")
	require.NoError(t, err)
	assert.Equal(t, oidc.Policy{Method: oidc.MethodBoth}, p)

	require.NoError(t, s.SetMethod(ctxA, "gina@example.com", oidc.MethodOIDC))
	assert.ErrorIs(t, s.SetMethod(ctxB, "gina@example.com", oidc.MethodOIDC), oidc.ErrNoSuchUser, "not a member of b")
	require.NoError(t, s.SetRequireOIDC(ctxA, true))
	p, err = s.Policy(ctxA, "gina@example.com")
	require.NoError(t, err)
	assert.Equal(t, oidc.Policy{RequireOIDC: true, Method: oidc.MethodOIDC}, p)
	req, err := s.RequireOIDC(ctxB)
	require.NoError(t, err)
	assert.False(t, req, "b is unchanged")
	methods, err := s.Methods(ctxA)
	require.NoError(t, err)
	assert.Equal(t, map[string]oidc.Method{"gina@example.com": oidc.MethodOIDC}, methods)

	// a transaction of b can't set a's policy
	_, err = d.App.Exec("SELECT set_config('rport.tenant_id', 'b', false); SELECT rport_set_require_oidc('a', false)")
	assert.Error(t, err)
	// the system tenant can't be changed
	err = s.SetRequireOIDC(tenant.WithID(context.Background(), "_system"), false)
	assert.Error(t, err)

	// unlinking needs the login to be a member of the tenant
	assert.ErrorIs(t, s.Unlink(ctxB, "gina@example.com"), oidc.ErrNoSuchUser)
	require.NoError(t, s.Unlink(ctxA, "gina@example.com"))
}
