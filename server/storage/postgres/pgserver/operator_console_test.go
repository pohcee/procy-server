package pgserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/operator"
	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/migrate"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	"github.com/openrport/openrport/share/tenant"
)

func mustExec(t *testing.T, db *sqlx.DB, q string, args ...any) {
	t.Helper()
	_, err := db.Exec(q, args...)
	require.NoError(t, err, q)
}

// consoleFixture: acme with devices of every kind, members, an invitation; beta from a signup.
func consoleFixture(t *testing.T, db, owner *sqlx.DB, o *Operator) {
	t.Helper()
	ctx := context.Background()
	for _, id := range []string{"acme", "beta"} {
		_, err := o.Create(ctx, ops, operator.NewOrganization{ID: id, Name: strings.ToUpper(id), DeviceLimit: intp(10)})
		require.NoError(t, err)
	}
	require.NoError(t, AddUser(ctx, db, "acme", "alice", testPassword, []string{"Administrators"}))
	require.NoError(t, AddUser(ctx, db, "acme", "carol", testPassword, nil))
	require.NoError(t, AddUser(ctx, db, "beta", "bob", testPassword, nil))
	// devices of acme: a role-all record (connected), a reported connected state, one disconnected an hour ago,
	// one with only a live session, and a lapsed session (not counted, not online)
	mustExec(t, owner, `INSERT INTO clients (tenant_id, id, client_auth_id, details) VALUES
		('acme', 'all-node', 'auth', '{"name":"Zed","version":"0.14.0","os":"linux","os_family":"debian","remote_address":"198.51.100.7","tags":["secret"]}')`)
	mustExec(t, owner, `INSERT INTO client_state (tenant_id, client_id, client_auth_id, disconnected_at, details, reported_at) VALUES
		('acme', 'state-on', 'auth', NULL, '{"name":"alpha","version":"0.15.0","os_full_name":"Ubuntu 24.04","os":"linux","os_family":"debian"}', now()),
		('acme', 'state-off', 'auth', now() - interval '1 hour', '{"name":"beta","version":"0.15.0","os":"windows"}', now())`)
	mustExec(t, owner, `INSERT INTO client_sessions (tenant_id, client_id, node_id, session_id, connected_at, lease_until) VALUES
		('acme', 'state-on', 'n1', 's1', now() - interval '10 minutes', now() + interval '1 minute'),
		('acme', 'session-only', 'n2', 's2', now() - interval '5 minutes', now() + interval '1 minute'),
		('acme', 'lapsed', 'n1', 's3', now() - interval '1 day', now() - interval '1 minute')`)
	// beta: one device of its own, which acme's reads must never show
	mustExec(t, owner, `INSERT INTO client_state (tenant_id, client_id, client_auth_id, disconnected_at, details, reported_at) VALUES
		('beta', 'beta-dev', 'auth', NULL, '{"name":"other"}', now())`)
	// bob (of beta) is invited to acme
	mustExec(t, owner, `INSERT INTO membership_invitations (tenant_id, username, groups, invited_by, created_at)
		VALUES ('acme', 'bob', '["Administrators"]', 'alice', now() - interval '2 days')`)
	mustExec(t, owner, `INSERT INTO signups (id, email, domain, status, expires_at, organization_id)
		VALUES ('s1', 'bob@example.com', 'example.com', 'completed', now() + interval '1 day', 'beta')`)
}

// countingDB is the application role through a connection that counts the statements sent.
func countingDB(t *testing.T, appURL string) (*sqlx.DB, *atomic.Int64) {
	t.Helper()
	cfg, err := pgx.ParseConfig(appURL)
	require.NoError(t, err)
	n := &atomic.Int64{}
	cfg.Tracer = statementCounter{n}
	db := sqlx.NewDb(stdlib.OpenDB(*cfg), postgres.DriverName)
	t.Cleanup(func() { db.Close() })
	return db, n
}

type statementCounter struct{ n *atomic.Int64 }

func (c statementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (statementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestConsoleOrganizations(t *testing.T) {
	o, d := newOperator(t)
	consoleFixture(t, d.App, d.Owner, o)
	ctx := context.Background()
	rec := NewLoginRecorder(d.App)
	require.NoError(t, rec.RecordLogin(tenant.WithID(ctx, "acme"), "alice"))

	orgs, err := o.Organizations(ctx)
	require.NoError(t, err)
	byID := map[string]operator.OrganizationDetail{}
	for _, x := range orgs {
		byID[x.ID] = x
	}
	require.Contains(t, byID, "acme")
	acme := byID["acme"]
	assert.Equal(t, 4, acme.DeviceCount, "all-node, state-on, state-off, session-only")
	assert.Equal(t, 3, acme.Activity.DevicesOnline, "all-node, state-on, session-only")
	assert.Equal(t, 2, acme.UserCount)
	assert.Equal(t, 1, acme.Activity.InvitationsPending)
	assert.False(t, acme.Activity.FromSignup)
	require.NotNil(t, acme.Activity.LastLoginAt)
	assert.WithinDuration(t, time.Now(), *acme.Activity.LastLoginAt, time.Minute)
	require.NotNil(t, acme.Activity.LastDeviceAt)
	assert.WithinDuration(t, time.Now(), *acme.Activity.LastDeviceAt, time.Minute, "a role-all record that is connected: now")

	beta := byID["beta"]
	assert.Equal(t, 1, beta.DeviceCount)
	assert.True(t, beta.Activity.FromSignup)
	assert.Nil(t, beta.Activity.LastLoginAt)

	// the list agrees with Get
	got, err := o.Get(ctx, "acme")
	require.NoError(t, err)
	assert.Equal(t, got.DeviceCount, acme.DeviceCount)
	assert.Equal(t, got.UserCount, acme.UserCount)
	list, err := o.List(ctx)
	require.NoError(t, err)
	assert.Len(t, list, len(orgs))
}

// The list takes the same number of statements for 3 organizations as for 25: no query per organization.
func TestConsoleOrganizationsInFixedStatements(t *testing.T) {
	d := pgtest.New(t)
	ctx := context.Background()
	setup := NewOperator(d.App, &chconfig.Config{API: chconfig.APIConfig{PasswordMinLength: 14}})
	db, n := countingDB(t, d.AppURL)
	o := NewOperator(db, &chconfig.Config{API: chconfig.APIConfig{PasswordMinLength: 14}})
	statements := func() int64 {
		n.Store(0)
		_, err := o.Organizations(ctx)
		require.NoError(t, err)
		_, err = o.List(ctx)
		require.NoError(t, err)
		return n.Load()
	}
	for i := 0; i < 3; i++ {
		_, err := setup.Create(ctx, ops, operator.NewOrganization{ID: fmt.Sprintf("org%d", i)})
		require.NoError(t, err)
	}
	few := statements()
	for i := 3; i < 25; i++ {
		_, err := setup.Create(ctx, ops, operator.NewOrganization{ID: fmt.Sprintf("org%d", i)})
		require.NoError(t, err)
		mustExec(t, d.Owner, `INSERT INTO client_state (tenant_id, client_id, client_auth_id, details, reported_at) VALUES ($1, 'c', 'a', '{}', now())`,
			fmt.Sprintf("org%d", i))
	}
	assert.Equal(t, few, statements())
	assert.LessOrEqual(t, few, int64(12), "begin, tenant, two reads, commit, twice")
}

// The functions that read across tenants refuse every caller outside the system tenant.
func TestConsoleFunctionsOnlyInSystemTenant(t *testing.T) {
	d := pgtest.New(t, "acme")
	ctx := context.Background()
	for _, q := range []string{
		"SELECT * FROM rport_operator_organization_counts(NULL)",
		"SELECT * FROM rport_operator_invitations(NULL)",
		"SELECT * FROM rport_operator_usage_days('2026-01-01', '2026-02-01')",
	} {
		err := postgres.InTenantID(ctx, d.App, "acme", func(tx *sqlx.Tx) error {
			_, err := tx.Exec(q)
			return err
		})
		assert.ErrorContains(t, err, "only by platform operators", q)
		_, err = d.App.Exec(q)
		assert.ErrorContains(t, err, "only by platform operators", "no tenant: %s", q)
		err = postgres.InTenantID(ctx, d.App, operator.SystemTenant, func(tx *sqlx.Tx) error {
			_, err := tx.Exec(q)
			return err
		})
		assert.NoError(t, err, q)
	}
	// the device role has none of them
	_, err := d.Device.Exec("SELECT * FROM rport_operator_organization_counts(NULL)")
	assert.ErrorContains(t, err, "permission denied")
}

func TestConsoleDevicesMetadataOnly(t *testing.T) {
	o, d := newOperator(t)
	consoleFixture(t, d.App, d.Owner, o)
	ctx := context.Background()
	ds, err := o.Devices(ctx, "acme")
	require.NoError(t, err)
	byID := map[string]operator.Device{}
	for _, x := range ds {
		byID[x.ID] = x
	}
	assert.Len(t, ds, 4)
	assert.NotContains(t, byID, "beta-dev", "another organization's device")
	assert.NotContains(t, byID, "lapsed")

	on := byID["state-on"]
	assert.True(t, on.Online)
	assert.Equal(t, "alpha", on.Name)
	assert.Equal(t, "0.15.0", on.Version)
	assert.Equal(t, "Ubuntu 24.04", on.OS, "the full OS name when there is one")
	assert.Equal(t, "debian", on.OSFamily)
	require.NotNil(t, on.NodeID)
	assert.Equal(t, "n1", *on.NodeID)
	require.NotNil(t, on.ConnectedAt)
	require.NotNil(t, on.FirstSeenAt)
	require.NotNil(t, on.LastSeenAt)

	all := byID["all-node"]
	assert.True(t, all.Online, "connected record without a session")
	assert.Nil(t, all.NodeID)
	assert.Nil(t, all.FirstSeenAt, "no state")
	assert.Equal(t, "linux", all.OS)

	off := byID["state-off"]
	assert.False(t, off.Online)
	require.NotNil(t, off.LastSeenAt)
	assert.WithinDuration(t, time.Now().Add(-time.Hour), *off.LastSeenAt, time.Minute)

	assert.True(t, byID["session-only"].Online)
	assert.Equal(t, "", byID["session-only"].Name)

	b, err := json.Marshal(ds)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "198.51.100.7")
	assert.NotContains(t, string(b), "secret")

	_, err = o.Devices(ctx, "nope")
	assert.ErrorIs(t, err, operator.ErrNotFound)
	_, err = o.Devices(ctx, "_system")
	assert.ErrorIs(t, err, operator.ErrNotFound)
}

func TestConsoleMembersAndLogins(t *testing.T) {
	cfg := &chconfig.Config{API: chconfig.APIConfig{PasswordMinLength: 14}}
	d := pgtest.New(t)
	o := NewOperator(d.App, cfg)
	consoleFixture(t, d.App, d.Owner, o)
	ctx := context.Background()
	mustExec(t, d.Owner, "UPDATE accounts SET totp_secret = 'x' WHERE username = 'carol'")
	mustExec(t, d.Owner, "INSERT INTO account_identities (issuer, subject, username) VALUES ('https://accounts.google.com', '1', 'alice')")

	var changes int
	require.NoError(t, d.Owner.Get(&changes, "SELECT count(*) FROM changes"))
	rec := NewLoginRecorder(d.App)
	require.NoError(t, rec.RecordLogin(tenant.WithID(ctx, "acme"), "alice"))
	require.NoError(t, rec.RecordLogin(tenant.WithID(ctx, "acme"), "bob"), "not a member: nothing is written")
	var after int
	require.NoError(t, d.Owner.Get(&after, "SELECT count(*) FROM changes"))
	assert.Equal(t, changes, after, "a sign-in is not a change for the nodes' caches")
	var accountLogin *time.Time
	require.NoError(t, d.Owner.Get(&accountLogin, "SELECT last_login_at FROM accounts WHERE username = 'alice'"))
	assert.NotNil(t, accountLogin)
	require.NoError(t, d.Owner.Get(&accountLogin, "SELECT last_login_at FROM accounts WHERE username = 'bob'"))
	assert.Nil(t, accountLogin)
	// any other update of users is still a change
	mustExec(t, d.Owner, "UPDATE users SET sign_in = 'password' WHERE username = 'carol'")
	require.NoError(t, d.Owner.Get(&after, "SELECT count(*) FROM changes"))
	assert.Equal(t, changes+1, after)

	ms, err := o.Members(ctx, "acme")
	require.NoError(t, err)
	require.Len(t, ms, 2)
	assert.Equal(t, "alice", ms[0].Username)
	assert.True(t, ms[0].HasPassword)
	assert.True(t, ms[0].OIDCLinked)
	assert.False(t, ms[0].TwoFA)
	require.NotNil(t, ms[0].LastLoginAt)
	assert.True(t, ms[1].TwoFA, "a TOTP secret")
	assert.False(t, ms[1].OIDCLinked)
	assert.Nil(t, ms[1].LastLoginAt)

	cfg.API.TwoFATokenDelivery = "smtp"
	ms, err = NewOperator(d.App, cfg).Members(ctx, "acme")
	require.NoError(t, err)
	assert.True(t, ms[0].TwoFA, "codes are sent to every login")

	// rport_record_login is for the application role only
	_, err = d.Device.Exec("SELECT rport_record_login('alice')")
	assert.ErrorContains(t, err, "permission denied")
}

func TestConsoleInvitations(t *testing.T) {
	o, d := newOperator(t)
	consoleFixture(t, d.App, d.Owner, o)
	ctx := context.Background()
	mustExec(t, d.Owner, `INSERT INTO membership_invitations (tenant_id, username, groups, invited_by) VALUES ('_system', 'bob', '[]', 'op')`)

	all, err := o.Invitations(ctx, "")
	require.NoError(t, err)
	require.Len(t, all, 1, "the system tenant's invitations are not an organization's")
	inv := all[0]
	assert.Equal(t, operator.Invitation{Organization: "acme", OrganizationName: "ACME", Username: "bob", Groups: []string{"Administrators"},
		InvitedBy: "alice", CreatedAt: inv.CreatedAt}, inv)
	only, err := o.Invitations(ctx, "beta")
	require.NoError(t, err)
	assert.Empty(t, only)
	_, err = o.Invitations(ctx, "nope")
	assert.ErrorIs(t, err, operator.ErrNotFound)

	renewed, err := o.ResendInvitation(ctx, ops, "acme", "bob")
	require.NoError(t, err)
	assert.Equal(t, "operator:op", renewed.InvitedBy)
	assert.True(t, renewed.CreatedAt.After(inv.CreatedAt))
	assert.Equal(t, []string{"Administrators"}, renewed.Groups)
	_, err = o.ResendInvitation(ctx, ops, "acme", "nobody")
	assert.ErrorIs(t, err, operator.ErrNoInvitation)

	require.NoError(t, o.RevokeInvitation(ctx, ops, "acme", "bob"))
	assert.ErrorIs(t, o.RevokeInvitation(ctx, ops, "acme", "bob"), operator.ErrNoInvitation)
	all, err = o.Invitations(ctx, "")
	require.NoError(t, err)
	assert.Empty(t, all)

	var audited []string
	for _, r := range systemAudit(t, d.App) {
		if r.Application == operator.AuditApplicationOrganizationInvitation {
			audited = append(audited, r.Action+" "+r.AffectedID+" "+r.Request)
		}
	}
	assert.Equal(t, []string{`resend acme {"username":"bob"}`, `delete acme {"username":"bob"}`}, audited)
}

func TestConsoleActivity(t *testing.T) {
	o, d := newOperator(t)
	consoleFixture(t, d.App, d.Owner, o)
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	insert := func(tid string, sec int, app, action, affected string) {
		mustExec(t, d.Owner, `INSERT INTO auditlog (tenant_id, timestamp, application, action, username, remote_ip, affected_id, request, response)
			VALUES ($1, $2, $3, $4, 'u', '192.0.2.9', $5, '{"password":"hunter2"}', '{"stdout":"secret"}')`,
			tid, base.Add(time.Duration(sec)*time.Second), app, action, affected)
	}
	mustExec(t, d.Owner, "DELETE FROM auditlog")
	insert("acme", 1, "auth.login", "success", "alice")
	insert("acme", 2, "client.command", "run", "c1")
	insert("acme", 3, "auth.user", "create", "carol")
	insert("acme", 4, "library.command", "create", "x")
	insert("acme", 5, "auth.oidc", "failed", "alice")
	insert("_system", 6, "operator.organization", "update", "acme")
	insert("_system", 7, "operator.organization", "update", "beta")
	insert("beta", 8, "auth.login", "success", "bob")
	// an operator whose login equals the organization id: its own sign-ins and user changes aren't the
	// organization's activity
	insert("_system", 9, "auth.login", "success", "acme")
	insert("_system", 10, "auth.user", "update", "acme")

	page := func(kind operator.ActivityKind, limit int, before *operator.ActivityCursor) []string {
		es, err := o.Activity(ctx, "acme", operator.ActivityQuery{Kind: kind, Limit: limit, Before: before})
		require.NoError(t, err)
		var out []string
		for _, e := range es {
			out = append(out, e.Source+" "+e.Application+" "+e.Action)
		}
		return out
	}
	assert.Equal(t, []string{
		"operator operator.organization update",
		"organization auth.oidc failed",
		"organization library.command create",
		"organization auth.user create",
		"organization client.command run",
		"organization auth.login success",
	}, page(operator.ActivityAll, 100, nil))
	assert.Equal(t, []string{"organization auth.oidc failed", "organization auth.login success"}, page(operator.ActivitySignIn, 100, nil))
	assert.Equal(t, []string{"operator operator.organization update", "organization auth.user create"}, page(operator.ActivityAdmin, 100, nil))
	assert.Len(t, page(operator.ActivityAll, 2, nil), 2)

	es, err := o.Activity(ctx, "acme", operator.ActivityQuery{Kind: operator.ActivityAll, Limit: 2})
	require.NoError(t, err)
	last := es[1]
	assert.Equal(t, "192.0.2.9", last.RemoteIP)
	assert.Equal(t, []string{"organization library.command create", "organization auth.user create"},
		page(operator.ActivityAll, 2, &operator.ActivityCursor{Timestamp: last.Timestamp, ID: last.ID}))
	b, err := json.Marshal(es)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "hunter2")
	assert.NotContains(t, string(b), "secret")

	_, err = o.Activity(ctx, "acme", operator.ActivityQuery{Kind: "x", Limit: 1})
	assert.ErrorIs(t, err, operator.ErrInvalid)
	_, err = o.Activity(ctx, "nope", operator.ActivityQuery{Limit: 1})
	assert.ErrorIs(t, err, operator.ErrNotFound)
}

func TestConsoleUsageDaysAndSignups(t *testing.T) {
	o, d := newOperator(t)
	consoleFixture(t, d.App, d.Owner, o)
	ctx := context.Background()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	y := today.AddDate(0, 0, -1)
	mustExec(t, d.Owner, `INSERT INTO usage_hourly (tenant_id, hour, metric, value) VALUES
		('acme', $1, 'tunnel_bytes_to_device', 10), ('acme', $1, 'upload_bytes_to_devices', 1),
		('acme', $1, 'tunnel_bytes_to_user', 20), ('beta', $2, 'api_bytes_out', 2), ('acme', $1, 'jobs_command', 5)`,
		y.Add(3*time.Hour), y.Add(4*time.Hour))
	mustExec(t, d.Owner, `INSERT INTO usage_device_days (tenant_id, day, client_id) VALUES ('acme', $1, 'a'), ('acme', $1, 'b'), ('beta', $2, 'c')`,
		y.Format(time.DateOnly), today.Format(time.DateOnly))
	days, err := o.UsageDays(ctx, y.AddDate(0, 0, -5), today.AddDate(0, 0, 1))
	require.NoError(t, err)
	assert.Equal(t, []operator.DayUsage{{Day: y, In: 11, Out: 22, Devices: 2}, {Day: today, Devices: 1}}, days)

	waiting, err := o.AwaitingSignups(ctx)
	require.NoError(t, err)
	assert.Empty(t, waiting)
	mustExec(t, d.Owner, `INSERT INTO signups (id, email, domain, status, expires_at, verified_at) VALUES
		('w1', 'x@example.com', 'example.com', 'awaiting_approval', now() + interval '1 day', now() - interval '2 days'),
		('w2', 'y@example.com', 'example.com', 'pending', now() + interval '1 day', NULL)`)
	waiting, err = o.AwaitingSignups(ctx)
	require.NoError(t, err)
	require.Len(t, waiting, 1)
	assert.WithinDuration(t, time.Now().Add(-48*time.Hour), waiting[0], time.Minute)
}

// With a schema owner that is not a superuser (row-level security forced on it), the set-based reads still see
// every organization: the cross_tenant_read policies cover the tables they read.
func TestConsoleFunctionsNonSuperuserOwner(t *testing.T) {
	ctx := context.Background()
	d := pgtest.NewEmpty(t)
	suffix := strings.TrimPrefix(d.Name, "t_")
	owner, app := "own_"+suffix, "app_"+suffix
	for _, stmt := range []string{
		"CREATE ROLE " + owner + " LOGIN PASSWORD 'pw' NOSUPERUSER NOBYPASSRLS",
		"CREATE ROLE " + app + " LOGIN PASSWORD 'pw' NOSUPERUSER NOBYPASSRLS",
		"ALTER DATABASE " + d.Name + " OWNER TO " + owner,
		"ALTER SCHEMA public OWNER TO " + owner,
	} {
		mustExec(t, d.Owner, stmt)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = d.Owner.ExecContext(bg, "ALTER DATABASE "+d.Name+" OWNER TO CURRENT_USER")
		_, _ = d.Owner.ExecContext(bg, "REASSIGN OWNED BY "+owner+" TO CURRENT_USER")
		_, _ = d.Owner.ExecContext(bg, "DROP OWNED BY "+owner+", "+app+" CASCADE")
		for _, r := range []string{app, owner} {
			_, _ = d.Owner.ExecContext(bg, "DROP ROLE IF EXISTS "+r)
		}
	})
	ownerDB := openRole(t, d.OwnerURL, owner)
	_, err := migrate.Up(ctx, ownerDB)
	require.NoError(t, err)
	require.NoError(t, migrate.GrantApp(ctx, ownerDB, app))
	appDB := openRole(t, d.OwnerURL, app)
	o := NewOperator(appDB, &chconfig.Config{API: chconfig.APIConfig{PasswordMinLength: 14}})
	consoleFixture(t, appDB, d.Owner, o)
	mustExec(t, d.Owner, `INSERT INTO client_admissions (tenant_id, client_id) VALUES ('beta', 'just-admitted')`)
	mustExec(t, d.Owner, `INSERT INTO usage_device_days (tenant_id, day, client_id) VALUES ('acme', current_date, 'a')`)

	orgs, err := o.Organizations(ctx)
	require.NoError(t, err)
	got := map[string][3]int{}
	for _, x := range orgs {
		got[x.ID] = [3]int{x.DeviceCount, x.Activity.DevicesOnline, x.UserCount}
	}
	assert.Equal(t, [3]int{4, 3, 2}, got["acme"])
	assert.Equal(t, [3]int{2, 1, 1}, got["beta"], "the admission counts")
	invs, err := o.Invitations(ctx, "")
	require.NoError(t, err)
	assert.Len(t, invs, 1)
	days, err := o.UsageDays(ctx, time.Now().AddDate(0, 0, -1), time.Now().AddDate(0, 0, 1))
	require.NoError(t, err)
	require.Len(t, days, 1)
	assert.Equal(t, int64(1), days[0].Devices)
}

func openRole(t *testing.T, rawURL, role string) *sqlx.DB {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	u.User = url.UserPassword(role, "pw")
	db, err := postgres.Open(context.Background(), u.String())
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}
