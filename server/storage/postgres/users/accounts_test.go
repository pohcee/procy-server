package users

import (
	"context"
	"net/http"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apierrors "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/repotest"
	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/migrate"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	"github.com/openrport/openrport/share/tenant"
)

func TestAccountSharedByMemberships(t *testing.T) {
	s, _ := newStore(t)
	a, b := repotest.Ctx(repotest.TenantA), repotest.Ctx(repotest.TenantB)
	require.NoError(t, s.Add(a, &users.User{Username: "carol", Password: "p1", TwoFASendTo: "a@x", Groups: []string{"Administrators"}}))

	// joining needs the existing account
	assert.ErrorIs(t, s.AddMember(b, &users.User{Username: "nobody"}), ErrNoAccount)
	require.NoError(t, s.AddMember(b, &users.User{Username: "carol", Password: "ignored", TwoFASendTo: "b@x", Groups: []string{"Viewers"}}))
	assert.ErrorIs(t, s.AddMember(b, &users.User{Username: "carol"}), ErrAlreadyMember)

	// one credential, groups and 2FA address per organization
	ua, err := s.GetByUsername(a, "carol")
	require.NoError(t, err)
	ub, err := s.GetByUsername(b, "carol")
	require.NoError(t, err)
	require.NotNil(t, ub)
	assert.Equal(t, "p1", ub.Password)
	assert.Equal(t, []string{"Administrators"}, ua.Groups)
	assert.Equal(t, []string{"Viewers"}, ub.Groups)
	assert.Equal(t, "a@x", ua.TwoFASendTo)
	assert.Equal(t, "b@x", ub.TwoFASendTo)

	// a credential change in one organization is the account's
	require.NoError(t, s.Update(b, &users.User{Password: "p2", PasswordExpired: users.PasswordExpired(true), TotP: "secret"}, "carol"))
	ua, err = s.GetByUsername(a, "carol")
	require.NoError(t, err)
	assert.Equal(t, "p2", ua.Password)
	assert.True(t, *ua.PasswordExpired)
	assert.Equal(t, "secret", ua.TotP)
	require.NoError(t, s.Update(a, &users.User{PasswordExpired: users.PasswordExpired(false)}, "carol"))
	ub, err = s.GetByUsername(b, "carol")
	require.NoError(t, err)
	assert.False(t, *ub.PasswordExpired, "password_expired can be cleared")
	all, err := s.GetAll(b)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "p2", all[0].Password)

	orgs, err := s.Organizations(context.Background(), "carol")
	require.NoError(t, err)
	assert.Equal(t, []Organization{{ID: "a", Name: "a"}, {ID: "b", Name: "b"}}, orgs)
	n, err := s.MembershipCount(context.Background(), "carol")
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	elsewhere, err := s.MemberElsewhere(a, "carol")
	require.NoError(t, err)
	assert.True(t, elsewhere)

	// a shared login can't be renamed by one organization
	err = s.Update(a, &users.User{Username: "caroline"}, "carol")
	var apiErr apierrors.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.HTTPStatus)
	assert.Contains(t, apiErr.Message, "other organizations")

	// removing one membership leaves the other and the account
	require.NoError(t, s.Delete(b, "carol"))
	ub, err = s.GetByUsername(b, "carol")
	require.NoError(t, err)
	assert.Nil(t, ub)
	ua, err = s.GetByUsername(a, "carol")
	require.NoError(t, err)
	require.NotNil(t, ua)
	assert.Equal(t, "p2", ua.Password)
	elsewhere, err = s.MemberElsewhere(a, "carol")
	require.NoError(t, err)
	assert.False(t, elsewhere)

	// a login of one organization alone renames its account
	require.NoError(t, s.Update(a, &users.User{Username: "caroline"}, "carol"))
	ua, err = s.GetByUsername(a, "caroline")
	require.NoError(t, err)
	require.NotNil(t, ua)
	assert.Equal(t, "p2", ua.Password)
	exists, err := s.AccountExists(context.Background(), "carol")
	require.NoError(t, err)
	assert.False(t, exists)

	// the last membership takes the account with it: the login is free again
	require.NoError(t, s.Delete(a, "caroline"))
	exists, err = s.AccountExists(context.Background(), "caroline")
	require.NoError(t, err)
	assert.False(t, exists)
	require.NoError(t, s.Add(b, &users.User{Username: "caroline", Password: "fresh"}))
}

func TestRememberOrganization(t *testing.T) {
	s, d := newStore(t)
	a, b := repotest.Ctx(repotest.TenantA), repotest.Ctx(repotest.TenantB)
	require.NoError(t, s.Add(a, &users.User{Username: "carol", Password: "p"}))
	require.NoError(t, s.Remember(b, "carol"), "not a member of b: nothing happens")
	last := func() *string {
		var v *string
		require.NoError(t, d.Owner.Get(&v, "SELECT last_tenant_id FROM accounts WHERE username = 'carol'"))
		return v
	}
	assert.Nil(t, last())
	require.NoError(t, s.AddMember(b, &users.User{Username: "carol"}))
	require.NoError(t, s.Remember(b, "carol"))
	require.NotNil(t, last())
	assert.Equal(t, "b", *last())
}

func TestAccountFunctionsRefuseDeviceRole(t *testing.T) {
	s, d := newStore(t)
	a := repotest.Ctx(repotest.TenantA)
	require.NoError(t, s.Add(a, &users.User{Username: "carol", Password: "p"}))
	for _, q := range []string{"SELECT rport_set_account('carol', 'x', NULL, NULL)", "SELECT rport_remember_tenant('carol')"} {
		err := postgres.InTenantID(context.Background(), d.Device, repotest.TenantA, func(tx *sqlx.Tx) error {
			_, err := tx.Exec(q)
			return err
		})
		assert.ErrorContains(t, err, "permission denied for function", q)
	}
	u, err := s.GetByUsername(a, "carol")
	require.NoError(t, err)
	assert.Equal(t, "p", u.Password)

	// the application role reads accounts but writes them only through the functions and triggers
	err = postgres.InTenantID(context.Background(), d.App, repotest.TenantA, func(tx *sqlx.Tx) error {
		_, err := tx.Exec("UPDATE accounts SET password = 'x'")
		return err
	})
	assert.ErrorContains(t, err, "permission denied")
}

func TestAPITokenLoginsFollowTokens(t *testing.T) {
	_, d := newStore(t)
	exec := func(id tenant.ID, q string) {
		t.Helper()
		require.NoError(t, postgres.InTenantID(context.Background(), d.App, id, func(tx *sqlx.Tx) error {
			_, err := tx.Exec(q)
			return err
		}))
	}
	exec(repotest.TenantA, "INSERT INTO api_tokens (tenant_id, username, prefix, token, name) VALUES ('a', 'carol', 'pfx1', 'h', 'n1')")
	exec(repotest.TenantB, "INSERT INTO api_tokens (tenant_id, username, prefix, token, name) VALUES ('b', 'carol', 'pfx2', 'h', 'n2')")
	var rows []string
	require.NoError(t, d.App.Select(&rows, "SELECT tenant_id || ':' || prefix FROM api_token_logins WHERE username = 'carol' ORDER BY 1"))
	assert.Equal(t, []string{"a:pfx1", "b:pfx2"}, rows)
	exec(repotest.TenantB, "DELETE FROM api_tokens WHERE tenant_id = 'b'")
	rows = nil
	require.NoError(t, d.App.Select(&rows, "SELECT tenant_id || ':' || prefix FROM api_token_logins WHERE username = 'carol' ORDER BY 1"))
	assert.Equal(t, []string{"a:pfx1"}, rows)
}

// Migration 1400 turns the users of a database migrated before it into accounts, and indexes its API tokens.
func TestAccountsMigrationBackfill(t *testing.T) {
	ctx := context.Background()
	d := pgtest.NewEmpty(t)
	all, err := migrate.All()
	require.NoError(t, err)
	_, err = d.Owner.Exec(`CREATE TABLE schema_migrations (version bigint PRIMARY KEY, name text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now())`)
	require.NoError(t, err)
	for _, m := range all {
		if m.Version >= 1400 {
			break
		}
		_, err := d.Owner.Exec(m.SQL)
		require.NoError(t, err, m.Name)
		_, err = d.Owner.Exec("INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.Version, m.Name)
		require.NoError(t, err)
	}
	_, err = d.Owner.Exec(`INSERT INTO tenants (id, name, status) VALUES ('a', 'A', 'active');
		INSERT INTO users (tenant_id, username, password, password_expired, two_fa_send_to, totp_secret)
			VALUES ('a', 'old', 'hash', true, 'old@x', 'totp');
		INSERT INTO user_groups (tenant_id, username, group_name) VALUES ('a', 'old', 'Administrators');
		INSERT INTO api_tokens (tenant_id, username, prefix, token, name) VALUES ('a', 'old', 'pfx1', 'h', 'n')`)
	require.NoError(t, err)

	_, err = migrate.Up(ctx, d.Owner)
	require.NoError(t, err)
	require.NoError(t, migrate.GrantApp(ctx, d.Owner, pgtest.AppRole))

	u, err := New(d.App).GetByUsername(repotest.Ctx("a"), "old")
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.Equal(t, "hash", u.Password)
	assert.True(t, *u.PasswordExpired)
	assert.Equal(t, "totp", u.TotP)
	assert.Equal(t, "old@x", u.TwoFASendTo)
	assert.Equal(t, []string{"Administrators"}, u.Groups)
	var stale int
	require.NoError(t, d.Owner.Get(&stale, "SELECT count(*) FROM users WHERE password <> '' OR totp_secret <> '' OR password_expired"))
	assert.Zero(t, stale, "one copy of the credential: the account's")
	var token string
	require.NoError(t, d.Owner.Get(&token, "SELECT tenant_id FROM api_token_logins WHERE username = 'old' AND prefix = 'pfx1'"))
	assert.Equal(t, "a", token)
}
