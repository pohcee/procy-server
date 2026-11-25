package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/operator"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	pgusers "github.com/openrport/openrport/server/storage/postgres/users"
	"github.com/openrport/openrport/share/tenant"
)

func org(t *testing.T, fn orgCommand, args ...string) (string, error) {
	t.Helper()
	var b bytes.Buffer
	err := runOrg(context.Background(), &b, fn, args)
	return b.String(), err
}

func TestOrgCommands(t *testing.T) {
	d := pgtest.New(t)
	// the application role, as a node connects: the operator functions are its only way to change tenants
	writePostgresConfig(t, d.AppURL)

	out, err := org(t, runOrgAdd, "acme", "Acme Corp")
	require.NoError(t, err)
	assert.Contains(t, out, `added organization "acme" (Acme Corp): status active, device limit unlimited`)

	setFlag(t, orgDeviceLimit, "25")
	out, err = org(t, runOrgAdd, "beta")
	require.NoError(t, err)
	assert.Contains(t, out, `device limit 25`)
	*orgDeviceLimit = "-1"
	_, err = org(t, runOrgAdd, "gamma")
	assert.ErrorContains(t, err, "whole number")
	*orgDeviceLimit = ""
	_, err = org(t, runOrgAdd, "acme")
	assert.ErrorIs(t, err, operator.ErrExists)

	out, err = org(t, runOrgSetLimit, "acme", "10")
	require.NoError(t, err)
	assert.Contains(t, out, "device limit 10")
	out, err = org(t, runOrgSetLimit, "beta", "unlimited")
	require.NoError(t, err)
	assert.Contains(t, out, "device limit unlimited")

	out, err = org(t, runOrgSuspend, "acme")
	require.NoError(t, err)
	assert.Contains(t, out, "status suspended")
	_, err = org(t, runOrgAddAdmin, "acme", "alice")
	assert.ErrorIs(t, err, operator.ErrNotActive)
	out, err = org(t, runOrgActivate, "acme")
	require.NoError(t, err)
	assert.Contains(t, out, "status active")

	out, err = org(t, runOrgAddAdmin, "acme", "alice")
	require.NoError(t, err)
	m := regexp.MustCompile(`password \(shown once; it must be changed at the first login\): (\S+)`).FindStringSubmatch(out)
	require.Len(t, m, 2, out)
	u, err := pgusers.New(d.App).GetByUsername(tenant.WithID(context.Background(), "acme"), "alice")
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.True(t, *u.PasswordExpired)

	out, err = org(t, runOrgAddAdmin, string(operator.SystemTenant), "ops")
	require.NoError(t, err, "the first platform operator")
	assert.Contains(t, out, `added administrator "ops" to "_system"`)

	// an existing account is invited: it joins when its owner accepts
	out, err = org(t, runOrgAddAdmin, "beta", "alice")
	require.NoError(t, err)
	assert.Contains(t, out, `invited "alice" to "beta" as an administrator`)
	assert.NotContains(t, out, "password (shown once")
	_, err = org(t, runOrgAddAdmin, "acme", "alice")
	assert.ErrorIs(t, err, operator.ErrExists, "a member already")

	out, err = org(t, runOrgList)
	require.NoError(t, err)
	assert.Regexp(t, `(?m)^ID\s+NAME\s+STATUS\s+DEVICES\s+LIMIT\s+USERS\s+CREATED$`, out)
	assert.Regexp(t, `(?m)^acme\s+Acme Corp\s+active\s+0\s+10\s+1\s+`, out)
	assert.Regexp(t, `(?m)^beta\s+beta\s+active\s+0\s+unlimited\s+0\s+`, out)
	assert.NotContains(t, out, "_system")

	*orgJSON = true
	t.Cleanup(func() { *orgJSON = false })
	out, err = org(t, runOrgList)
	require.NoError(t, err)
	var orgs []operator.Organization
	require.NoError(t, json.Unmarshal([]byte(out), &orgs), out)
	assert.Len(t, orgs, 3)

	_, err = org(t, runOrgSuspend, "nope")
	assert.ErrorIs(t, err, operator.ErrNotFound)

	var actors []string
	require.NoError(t, d.Owner.Select(&actors, "SELECT DISTINCT username FROM auditlog WHERE tenant_id = '_system'"))
	require.Len(t, actors, 1)
	assert.Regexp(t, `^cli:`, actors[0], "the audit log names the operating system user")
	var n int
	require.NoError(t, d.Owner.Get(&n, "SELECT count(*) FROM auditlog WHERE tenant_id = '_system'"))
	assert.Equal(t, 9, n, "2 adds, 2 limits, suspend, activate and 3 admins; failures are not logged")
}

func TestOrgNeedsPostgres(t *testing.T) {
	writeMigrateConfig(t, filepath.Join(t.TempDir(), "data"))
	_, err := org(t, runOrgList)
	assert.ErrorContains(t, err, `organizations exist only with [storage] backend = "postgres"`)
}

func TestOrgRefusesRoot(t *testing.T) {
	asRoot(t)
	writePostgresConfig(t, "postgres://rport_app@127.0.0.1:1/none")
	_, err := org(t, runOrgList)
	assert.ErrorContains(t, err, "root")
}

func TestParseLimit(t *testing.T) {
	for in, want := range map[string]*int{"": nil, "unlimited": nil, "none": nil, "0": new(int)} {
		got, err := parseLimit(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"-1", "x", "1.5"} {
		_, err := parseLimit(bad)
		assert.Error(t, err, bad)
	}
}

func TestOrgAdminsWithOIDC(t *testing.T) {
	d := pgtest.New(t)
	keyFile := filepath.Join(t.TempDir(), "vault.key")
	require.NoError(t, os.WriteFile(keyFile, []byte(strings.Repeat("ab", 32)+"\n"), 0600))
	secret := filepath.Join(t.TempDir(), "oidc-secret")
	require.NoError(t, os.WriteFile(secret, []byte("s3cret\n"), 0600))
	writeConfig(t, "[server]\n  data_dir = \""+t.TempDir()+"\"\n"+
		"[storage]\n  backend = \"postgres\"\n  dsn = \""+d.AppURL+"\"\n"+
		"[vault]\n  key_provider = \"local\"\n  local_key_file = \""+keyFile+"\"\n"+
		"[oidc]\n  enabled = true\n  client_id = \"cid\"\n  client_secret_file = \""+secret+"\"\n"+
		"  redirect_url = \"https://procy.example/api/v1/oidc/callback\"\n")

	// an operator gets no password at all
	out, err := org(t, runOrgAddAdmin, string(operator.SystemTenant), "ops@example.com")
	require.NoError(t, err)
	assert.Contains(t, out, `added administrator "ops@example.com" to "_system" without a password`)
	assert.NotContains(t, out, "password (shown once")
	var hash string
	require.NoError(t, d.Owner.Get(&hash, "SELECT password FROM accounts WHERE username = 'ops@example.com'"))
	assert.Empty(t, hash)

	// organizations that don't require OIDC still give a password
	_, err = org(t, runOrgAdd, "acme")
	require.NoError(t, err)
	out, err = org(t, runOrgAddAdmin, "acme", "alice@example.com")
	require.NoError(t, err)
	assert.Contains(t, out, "password (shown once")
	_, err = org(t, runOrgAddAdmin, "acme", "bob@example.com")
	require.NoError(t, err)

	// remove-admin: the last administrator stays unless forced
	_, err = org(t, runOrgRemoveAdmin, string(operator.SystemTenant), "ops@example.com")
	assert.ErrorIs(t, err, operator.ErrLastAdmin)
	_, err = org(t, runOrgRemoveAdmin, "acme", "nobody@example.com")
	assert.ErrorIs(t, err, operator.ErrNotMember)
	out, err = org(t, runOrgRemoveAdmin, "acme", "bob@example.com")
	require.NoError(t, err)
	assert.Contains(t, out, `removed administrator "bob@example.com" from "acme"`)
	var n int
	require.NoError(t, d.Owner.Get(&n, "SELECT count(*) FROM accounts WHERE username = 'bob@example.com'"))
	assert.Zero(t, n, "its last membership: the account is gone")
	_, err = org(t, runOrgRemoveAdmin, "acme", "alice@example.com")
	assert.ErrorIs(t, err, operator.ErrLastAdmin)
	setBool(t, orgForce, true)
	_, err = org(t, runOrgRemoveAdmin, string(operator.SystemTenant), "ops@example.com")
	require.NoError(t, err)
	_, err = org(t, runOrgRemoveAdmin, "nope", "x")
	assert.ErrorIs(t, err, operator.ErrNotFound)
}

func setBool(t *testing.T, flag *bool, value bool) {
	t.Helper()
	prev := *flag
	*flag = value
	t.Cleanup(func() { *flag = prev })
}
