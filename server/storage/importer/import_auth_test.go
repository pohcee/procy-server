package importer

import (
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/clientsauth"
	"github.com/openrport/openrport/server/repotest"
	pgclientauth "github.com/openrport/openrport/server/storage/postgres/clientauth"
	"github.com/openrport/openrport/server/storage/postgres/pgserver"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	pgusers "github.com/openrport/openrport/server/storage/postgres/users"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
)

func hash(t *testing.T, pass string) string {
	t.Helper()
	h, err := users.GenerateTokenHash(pass)
	require.NoError(t, err)
	return h
}

// sortedUsers returns the users with their groups sorted, as plain values.
func sortedUsers(us []*users.User) []users.User {
	out := make([]users.User, 0, len(us))
	for _, u := range us {
		c := *u
		c.Groups = append([]string(nil), u.Groups...)
		sort.Strings(c.Groups)
		if c.PasswordExpired == nil {
			c.PasswordExpired = users.PasswordExpired(false)
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

func TestImportUsersFromAuthFile(t *testing.T) {
	pg := pgtest.New(t)
	d := newDataDir(t)
	// the config names the paths of the original server: they are read from the imported data dir
	d.write("api-auth.json", fmt.Sprintf(`[
		{"username": "admin", "password": %q, "groups": ["Administrators"], "password_expired": true, "two_fa_send_to": "admin@example.com"},
		{"username": "bob", "password": %q, "groups": ["ops", "viewers"], "totp_secret": "{\"secret\":\"abc\"}"}
	]`, hash(t, "admin-pass"), hash(t, "bob-pass")))
	conf := filepath.Join(t.TempDir(), "rportd.conf")
	writeFile(t, conf, `[server]
data_dir = "/var/lib/rport"
[api]
auth_file = "/var/lib/rport/api-auth.json"
`)
	wantUsers, err := users.NewFileManager(testLog, d.path("api-auth.json")).ReadUsersFromFile()
	require.NoError(t, err)

	rep := run(t, pg, Options{From: d.dir, FromConfig: conf})
	want(t, rep, entUsers, 2, 2, 0, 0)
	want(t, rep, entUserGroups, 3, 3, 0, 0)
	want(t, rep, entGroupPermissions, 0, 0, 0, 0)
	assert.True(t, hasWarning(rep, "no client authentication"), rep.Warnings)

	got, err := pgusers.New(pg.App).GetAll(ctxDefault)
	require.NoError(t, err)
	assert.Equal(t, sortedUsers(wantUsers), sortedUsers(got))
	groups, err := pgusers.New(pg.App).ListGroups(ctxDefault)
	require.NoError(t, err)
	sameJSON(t, []users.Group{users.NewGroup("Administrators", nil, nil), users.NewGroup("ops", nil, nil), users.NewGroup("viewers", nil, nil)}, groups)
}

func TestImportUsersFromDatabaseTable(t *testing.T) {
	pg := pgtest.New(t, "other")
	d := newDataDir(t)
	// a user of another tenant has the login "taken": logins are unique across tenants
	require.NoError(t, pgserver.AddUser(ctxDefault, pg.App, "other", "taken", "pass-pass-pass", nil))

	authDB := d.path("auth.sqlite")
	db, err := sqlite.Open(authDB, sqlite.DataSourceOptions{})
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE users (username TEXT PRIMARY KEY, password TEXT NOT NULL, password_expired BOOLEAN NOT NULL DEFAULT 0, two_fa_send_to TEXT, totp_secret TEXT);
		CREATE TABLE groups (username TEXT NOT NULL, "group" TEXT NOT NULL, UNIQUE (username, "group"));
		CREATE TABLE group_details (name TEXT PRIMARY KEY, permissions TEXT NOT NULL DEFAULT '{}', tunnels_restricted TEXT, commands_restricted TEXT);
		CREATE TABLE clients_auth (id TEXT PRIMARY KEY, password TEXT NOT NULL);`)
	require.NoError(t, err)
	udb, err := users.NewUserDatabase(db, "users", "groups", "group_details", true, true, true, testLog)
	require.NoError(t, err)
	for _, u := range []*users.User{
		{Username: "alice", Password: hash(t, "a"), Groups: []string{"Administrators"}, TwoFASendTo: "alice@example.com"},
		{Username: "carol", Password: hash(t, "c"), Groups: []string{"ops"}, TotP: "totp"},
		{Username: "taken", Password: hash(t, "t"), Groups: []string{"ops"}},
	} {
		require.NoError(t, udb.Add(ctxDefault, u))
	}
	_, err = db.Exec("UPDATE users SET password_expired = 1 WHERE username = 'carol'")
	require.NoError(t, err)
	require.NoError(t, udb.UpdateGroup(ctxDefault, "ops", users.NewGroup("ops", nil, nil, users.PermissionCommands, users.PermissionTunnels)))
	_, err = db.Exec(`INSERT INTO group_details (name, permissions, tunnels_restricted) VALUES ('limited', '{"tunnels":true}', '{"scheme":["ssh"]}')`)
	require.NoError(t, err)
	ca := clientsauth.NewDatabaseProvider(db, "clients_auth")
	_, err = ca.Add(ctxDefault, &clientsauth.ClientAuth{ID: "dev-1", Password: "plain-1"})
	require.NoError(t, err)
	wantUsers, err := udb.GetAll(ctxDefault)
	require.NoError(t, err)
	wantGroups, err := udb.ListGroups(ctxDefault)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	conf := filepath.Join(t.TempDir(), "rportd.conf")
	writeFile(t, conf, fmt.Sprintf(`[server]
auth_table = "clients_auth"
[api]
auth_user_table = "users"
auth_group_table = "groups"
auth_group_details_table = "group_details"
[database]
db_type = "sqlite"
db_name = %q
`, authDB))

	rep := run(t, pg, Options{From: d.dir, FromConfig: conf})
	want(t, rep, entUsers, 3, 2, 0, 1)
	want(t, rep, entUserGroups, 3, 2, 0, 1)
	want(t, rep, entGroupPermissions, 2, 2, 0, 0)
	want(t, rep, entClientAuth, 1, 1, 0, 0)
	assert.True(t, hasWarning(rep, "usernames used by another tenant, not imported: taken"), rep.Warnings)

	got, err := pgusers.New(pg.App).GetAll(ctxDefault)
	require.NoError(t, err)
	assert.Equal(t, sortedUsers(wantUsers[:2]), sortedUsers(got))
	gotGroups, err := pgusers.New(pg.App).ListGroups(ctxDefault)
	require.NoError(t, err)
	sameJSON(t, wantGroups, gotGroups)
	cred, err := pgclientauth.New(pg.App).Get(ctxDefault, "dev-1")
	require.NoError(t, err)
	assert.Equal(t, &clientsauth.ClientAuth{ID: "dev-1", Password: "plain-1"}, cred)
}

func TestImportClientAuthFile(t *testing.T) {
	pg := pgtest.New(t, "other")
	d := newDataDir(t)
	// another tenant has the id "dev-taken": client auth ids are unique across tenants
	_, err := pgclientauth.New(pg.App).Add(repotest.Ctx("other"), &clientsauth.ClientAuth{ID: "dev-taken", Password: "x"})
	require.NoError(t, err)
	d.write("client-auth.json", `{"dev-1": "plain-1", "dev-2": "$2y$05$abcdefghijklmnopqrstuuJ0Qy8zv0L0L0L0L0L0L0L0L0L0L0L0L", "dev-taken": "y"}`)
	conf := filepath.Join(t.TempDir(), "rportd.conf")
	writeFile(t, conf, `[server]
data_dir = "/var/lib/rport"
auth_file = "/var/lib/rport/client-auth.json"
[api]
auth = "admin:static-pass"
`)
	wantCreds, _, err := clientsauth.NewFileProvider(d.path("client-auth.json"), nil).GetFiltered(ctxDefault, &query.ListOptions{Pagination: query.NewPagination(100, 0)})
	require.NoError(t, err)

	rep := run(t, pg, Options{From: d.dir, FromConfig: conf})
	want(t, rep, entClientAuth, 3, 2, 0, 1)
	assert.True(t, hasWarning(rep, "client auth: ids used by another tenant, not imported: dev-taken"), rep.Warnings)
	got, _, err := pgclientauth.New(pg.App).GetFiltered(ctxDefault, &query.ListOptions{Pagination: query.NewPagination(100, 0)})
	require.NoError(t, err)
	assert.Equal(t, wantCreds[:2], got)

	// the static [api] auth user is stored with a bcrypt hash of its password
	want(t, rep, entUsers, 1, 1, 0, 0)
	admin, err := pgusers.New(pg.App).GetByUsername(ctxDefault, "admin")
	require.NoError(t, err)
	require.NotNil(t, admin)
	assert.Equal(t, []string{users.Administrators}, admin.Groups)
	assert.Regexp(t, `^\$2y\$`, admin.Password)
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(admin.Password), []byte("static-pass")))
}

func TestImportStaticClientAuth(t *testing.T) {
	pg := pgtest.New(t)
	d := newDataDir(t)
	conf := filepath.Join(t.TempDir(), "rportd.conf")
	writeFile(t, conf, "[server]\nauth = \"dev-1:secret\"\n")
	rep := run(t, pg, Options{From: d.dir, FromConfig: conf})
	want(t, rep, entClientAuth, 1, 1, 0, 0)
	assert.True(t, hasWarning(rep, "no API user store"), rep.Warnings)
	cred, err := pgclientauth.New(pg.App).Get(tenant.WithID(ctxDefault, tenant.Default), "dev-1")
	require.NoError(t, err)
	assert.Equal(t, &clientsauth.ClientAuth{ID: "dev-1", Password: "secret"}, cred)
}

func TestImportWithoutConfig(t *testing.T) {
	pg := pgtest.New(t)
	rep := run(t, pg, Options{From: newDataDir(t).dir})
	assert.Nil(t, rep.Get(entUsers))
	assert.Nil(t, rep.Get(entClientAuth))
	assert.True(t, hasWarning(rep, "no source config given: users and client credentials are not imported"), rep.Warnings)
}
