package pgserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/api/users"
	"github.com/pohcee/procy-server/server/chconfig"
	"github.com/pohcee/procy-server/server/operator"
	"github.com/pohcee/procy-server/server/storage/postgres"
	"github.com/pohcee/procy-server/server/storage/postgres/pgtest"
	pgusers "github.com/pohcee/procy-server/server/storage/postgres/users"
	"github.com/pohcee/procy-server/share/tenant"
)

var ops = operator.Actor{Username: "op", RemoteIP: "192.0.2.1"}

func newOperator(t *testing.T) (*Operator, *pgtest.DB) {
	t.Helper()
	d := pgtest.New(t)
	return NewOperator(d.App, &chconfig.Config{API: chconfig.APIConfig{PasswordMinLength: 14}}), d
}

func intp(n int) *int { return &n }

type auditRow struct {
	Username    string `db:"username"`
	RemoteIP    string `db:"remote_ip"`
	Application string `db:"application"`
	Action      string `db:"action"`
	AffectedID  string `db:"affected_id"`
	Request     string `db:"request"`
}

func systemAudit(t *testing.T, db *sqlx.DB) []auditRow {
	t.Helper()
	var rows []auditRow
	require.NoError(t, postgres.InTenantID(context.Background(), db, operator.SystemTenant, func(tx *sqlx.Tx) error {
		return tx.Select(&rows, `SELECT coalesce(username, '') AS username, coalesce(remote_ip, '') AS remote_ip, application,
			action, coalesce(affected_id, '') AS affected_id, coalesce(request, '') AS request FROM auditlog WHERE tenant_id = '_system' ORDER BY id`)
	}))
	return rows
}

func TestSystemTenantIsActiveAndHasNoDevices(t *testing.T) {
	d := pgtest.New(t)
	var status string
	require.NoError(t, d.Owner.Get(&status, "SELECT status FROM tenants WHERE id = '_system'"))
	assert.Equal(t, "active", status)

	err := postgres.InTenantID(context.Background(), d.App, operator.SystemTenant, func(tx *sqlx.Tx) error {
		_, err := tx.Exec("INSERT INTO clients_auth (tenant_id, id, password) VALUES ('_system', 'dev1', 'pw')")
		return err
	})
	assert.ErrorContains(t, err, "system tenant has no clients_auth")
	err = postgres.InTenantID(context.Background(), d.App, operator.SystemTenant, func(tx *sqlx.Tx) error {
		_, err := tx.Exec("INSERT INTO clients (tenant_id, id, client_auth_id, details) VALUES ('_system', 'c1', 'a', '{}')")
		return err
	})
	assert.ErrorContains(t, err, "system tenant has no clients")
}

func TestOperatorFunctionsOnlyInSystemTenant(t *testing.T) {
	d := pgtest.New(t)
	ctx := context.Background()
	for _, tid := range []tenant.ID{tenant.Default, "_unresolved"} {
		err := postgres.InTenantID(ctx, d.App, tid, func(tx *sqlx.Tx) error {
			_, err := tx.Exec("SELECT rport_operator_add_tenant('acme', 'Acme', NULL)")
			return err
		})
		assert.ErrorContains(t, err, "only by platform operators", tid)
	}
	_, err := d.App.Exec("SELECT rport_operator_set_tenant_status('default', 'suspended')")
	assert.ErrorContains(t, err, "only by platform operators", "no tenant set")

	_, err = d.App.Exec("UPDATE tenants SET status = 'suspended' WHERE id = 'default'")
	assert.ErrorContains(t, err, "permission denied", "the application role still can't write tenants directly")

	err = postgres.InTenantID(ctx, d.App, operator.SystemTenant, func(tx *sqlx.Tx) error {
		_, err := tx.Exec("SELECT rport_operator_set_tenant_status('_system', 'suspended')")
		return err
	})
	assert.ErrorContains(t, err, "is reserved")
}

func TestOperatorCreateListGet(t *testing.T) {
	o, d := newOperator(t)
	ctx := context.Background()

	org, err := o.Create(ctx, ops, operator.NewOrganization{ID: "acme", Name: " Acme Corp ", DeviceLimit: intp(10)})
	require.NoError(t, err)
	assert.Equal(t, "acme", org.ID)
	assert.Equal(t, "Acme Corp", org.Name)
	assert.Equal(t, operator.StatusActive, org.Status)
	assert.Equal(t, intp(10), org.DeviceLimit)
	assert.WithinDuration(t, time.Now(), org.CreatedAt, time.Minute)

	_, err = o.Create(ctx, ops, operator.NewOrganization{ID: "beta"})
	require.NoError(t, err)
	require.NoError(t, AddUser(ctx, d.App, "beta", "bob", testPassword, nil))

	orgs, err := o.List(ctx)
	require.NoError(t, err)
	var ids []string
	for _, x := range orgs {
		ids = append(ids, x.ID)
	}
	assert.Equal(t, []string{"default", "acme", "beta"}, ids, "reserved and system tenants are not organizations")
	assert.Equal(t, "beta", orgs[2].Name, "name defaults to the id")
	assert.Nil(t, orgs[2].DeviceLimit, "unlimited")
	assert.Equal(t, 1, orgs[2].UserCount)
	assert.Equal(t, 0, orgs[2].DeviceCount)

	got, err := o.Get(ctx, "acme")
	require.NoError(t, err)
	assert.Equal(t, org.ID, got.ID)

	var n int
	require.NoError(t, postgres.InTenantID(ctx, d.App, "acme", func(tx *sqlx.Tx) error {
		return tx.Get(&n, "SELECT count(*) FROM library_entries WHERE tenant_id = 'acme'")
	}))
	assert.Positive(t, n, "built-in library entries")

	_, err = o.Create(ctx, ops, operator.NewOrganization{ID: "acme"})
	assert.ErrorIs(t, err, operator.ErrExists)
	_, err = o.Create(ctx, ops, operator.NewOrganization{ID: "default"})
	assert.ErrorIs(t, err, operator.ErrExists)
	for _, bad := range []string{"", "_x", "_system", "Acme", "a b", "-a"} {
		_, err = o.Create(ctx, ops, operator.NewOrganization{ID: bad})
		assert.ErrorIs(t, err, operator.ErrInvalid, bad)
	}
	_, err = o.Create(ctx, ops, operator.NewOrganization{ID: "neg", DeviceLimit: intp(-1)})
	assert.ErrorIs(t, err, operator.ErrInvalid)

	for _, id := range []string{"nope", "_system", "_unresolved"} {
		_, err = o.Get(ctx, id)
		assert.ErrorIs(t, err, operator.ErrNotFound, id)
	}
}

func TestOperatorSuspendBlocksLogins(t *testing.T) {
	o, d := newOperator(t)
	ctx := context.Background()
	_, err := o.Create(ctx, ops, operator.NewOrganization{ID: "acme"})
	require.NoError(t, err)
	require.NoError(t, AddUser(ctx, d.App, "acme", "alice", testPassword, nil))
	r := NewResolver(d.App, testSecret)
	login := func() tenant.ID {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/clients", nil)
		req.SetBasicAuth("alice", "x")
		id, err := r.Resolve(req)
		require.NoError(t, err)
		return id
	}
	tok := token(t, "acme", testSecret)
	bearer := func() tenant.ID {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/clients", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		id, err := r.Resolve(req)
		require.NoError(t, err)
		return id
	}
	assert.Equal(t, tenant.ID("acme"), login())
	assert.Equal(t, tenant.ID("acme"), bearer())

	org, err := o.SetStatus(ctx, ops, "acme", operator.StatusSuspended)
	require.NoError(t, err)
	assert.Equal(t, operator.StatusSuspended, org.Status)
	assert.Equal(t, Unresolved, login(), "a suspended organization's logins stop at once")
	assert.Equal(t, Unresolved, bearer(), "and its sessions")

	_, err = o.AddAdmin(ctx, ops, "acme", operator.NewAdmin{Username: "carol"})
	assert.ErrorIs(t, err, operator.ErrNotActive)

	org, err = o.SetStatus(ctx, ops, "acme", operator.StatusActive)
	require.NoError(t, err)
	assert.Equal(t, operator.StatusActive, org.Status)
	assert.Equal(t, tenant.ID("acme"), login())

	_, err = o.SetStatus(ctx, ops, "nope", operator.StatusSuspended)
	assert.ErrorIs(t, err, operator.ErrNotFound)
	_, err = o.SetStatus(ctx, ops, "_system", operator.StatusSuspended)
	assert.ErrorIs(t, err, operator.ErrNotFound)
	_, err = o.SetStatus(ctx, ops, "acme", "reserved")
	assert.ErrorIs(t, err, operator.ErrInvalid)
}

func TestOperatorSetDeviceLimit(t *testing.T) {
	o, _ := newOperator(t)
	ctx := context.Background()
	_, err := o.Create(ctx, ops, operator.NewOrganization{ID: "acme"})
	require.NoError(t, err)

	org, err := o.SetDeviceLimit(ctx, ops, "acme", intp(5))
	require.NoError(t, err)
	assert.Equal(t, intp(5), org.DeviceLimit)
	got, err := o.Get(ctx, "acme")
	require.NoError(t, err)
	assert.Equal(t, intp(5), got.DeviceLimit, "read back")

	org, err = o.SetDeviceLimit(ctx, ops, "acme", nil)
	require.NoError(t, err)
	assert.Nil(t, org.DeviceLimit)

	_, err = o.SetDeviceLimit(ctx, ops, "acme", intp(-2))
	assert.ErrorIs(t, err, operator.ErrInvalid)
	_, err = o.SetDeviceLimit(ctx, ops, "nope", intp(1))
	assert.ErrorIs(t, err, operator.ErrNotFound)
	_, err = o.SetDeviceLimit(ctx, ops, "_system", intp(1))
	assert.ErrorIs(t, err, operator.ErrNotFound)
}

func TestOperatorAddAdmin(t *testing.T) {
	o, d := newOperator(t)
	ctx := context.Background()
	_, err := o.Create(ctx, ops, operator.NewOrganization{ID: "acme"})
	require.NoError(t, err)

	a, err := o.AddAdmin(ctx, ops, "acme", operator.NewAdmin{Username: "alice@example.com"})
	require.NoError(t, err)
	assert.Equal(t, "acme", a.Organization)
	assert.Equal(t, "alice@example.com", a.Username)
	assert.Len(t, a.Password, generatedPasswordLength)
	assert.True(t, a.PasswordExpired)

	u, err := pgusers.New(d.App).GetByUsername(tenant.WithID(ctx, "acme"), "alice@example.com")
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.Equal(t, []string{users.Administrators}, u.Groups)
	require.NotNil(t, u.PasswordExpired)
	assert.True(t, *u.PasswordExpired, "the first login must set a new password")
	assert.NotEqual(t, a.Password, u.Password, "stored hashed")

	a2, err := o.AddAdmin(ctx, ops, "acme", operator.NewAdmin{Username: "dave"})
	require.NoError(t, err)
	assert.NotEqual(t, a.Password, a2.Password)

	_, err = o.AddAdmin(ctx, ops, "acme", operator.NewAdmin{Username: "alice@example.com"})
	assert.ErrorIs(t, err, operator.ErrExists)
	// an existing account is invited, and joins with its own password when its owner accepts
	joined, err := o.AddAdmin(ctx, ops, "default", operator.NewAdmin{Username: "alice@example.com"})
	require.NoError(t, err)
	assert.Equal(t, operator.Admin{Organization: "default", Username: "alice@example.com", Pending: true}, *joined)
	ud, err := pgusers.New(d.App).GetByUsername(tenant.WithID(ctx, "default"), "alice@example.com")
	require.NoError(t, err)
	assert.Nil(t, ud, "not a member before accepting")
	require.NoError(t, pgusers.New(d.App).AcceptInvitation(tenant.WithID(ctx, "default"), "alice@example.com"))
	ud, err = pgusers.New(d.App).GetByUsername(tenant.WithID(ctx, "default"), "alice@example.com")
	require.NoError(t, err)
	require.NotNil(t, ud)
	assert.Equal(t, u.Password, ud.Password, "one credential")
	assert.Equal(t, []string{users.Administrators}, ud.Groups)
	members, err := o.Members(ctx, "acme")
	require.NoError(t, err)
	assert.Equal(t, []operator.Member{
		{Username: "alice@example.com", Groups: []string{users.Administrators}, Organizations: 2, SignIn: "both", HasPassword: true},
		{Username: "dave", Groups: []string{users.Administrators}, Organizations: 1, SignIn: "both", HasPassword: true},
	}, members)
	_, err = o.Members(ctx, "nope")
	assert.ErrorIs(t, err, operator.ErrNotFound)
	_, err = o.AddAdmin(ctx, ops, "acme", operator.NewAdmin{Username: ""})
	assert.Error(t, err)

	op, err := o.AddAdmin(ctx, ops, string(operator.SystemTenant), operator.NewAdmin{Username: "op2"})
	require.NoError(t, err, "another platform operator")
	assert.Equal(t, "_system", op.Organization)
	for _, id := range []string{"nope", "_unresolved"} {
		_, err = o.AddAdmin(ctx, ops, id, operator.NewAdmin{Username: "x"})
		assert.ErrorIs(t, err, operator.ErrNotFound, id)
	}
}

func TestOperatorAuditLog(t *testing.T) {
	o, d := newOperator(t)
	ctx := context.Background()
	_, err := o.Create(ctx, ops, operator.NewOrganization{ID: "acme", DeviceLimit: intp(3)})
	require.NoError(t, err)
	_, err = o.SetDeviceLimit(ctx, ops, "acme", intp(4))
	require.NoError(t, err)
	_, err = o.SetStatus(ctx, ops, "acme", operator.StatusSuspended)
	require.NoError(t, err)
	_, err = o.SetStatus(ctx, ops, "acme", operator.StatusActive)
	require.NoError(t, err)
	a, err := o.AddAdmin(ctx, ops, "acme", operator.NewAdmin{Username: "alice"})
	require.NoError(t, err)
	// failures are not logged
	_, _ = o.SetStatus(ctx, ops, "nope", operator.StatusSuspended)

	rows := systemAudit(t, d.App)
	require.Len(t, rows, 5)
	want := []struct{ app, action string }{
		{operator.AuditApplicationOrganization, operator.AuditActionCreate},
		{operator.AuditApplicationOrganization, operator.AuditActionUpdate},
		{operator.AuditApplicationOrganization, operator.AuditActionSuspend},
		{operator.AuditApplicationOrganization, operator.AuditActionActivate},
		{operator.AuditApplicationOrganizationAdmin, operator.AuditActionCreate},
	}
	for i, w := range want {
		assert.Equal(t, w.app, rows[i].Application, i)
		assert.Equal(t, w.action, rows[i].Action, i)
		assert.Equal(t, "acme", rows[i].AffectedID, i)
		assert.Equal(t, "op", rows[i].Username, i)
		assert.Equal(t, "192.0.2.1", rows[i].RemoteIP, i)
		assert.NotContains(t, rows[i].Request, a.Password, "the generated password is never logged")
	}
	assert.JSONEq(t, `{"id":"acme","name":"acme","device_limit":3}`, rows[0].Request)
	assert.JSONEq(t, `{"device_limit":4}`, rows[1].Request)
	assert.JSONEq(t, `{"username":"alice"}`, rows[4].Request)

	var n int
	require.NoError(t, postgres.InTenantID(ctx, d.App, "acme", func(tx *sqlx.Tx) error {
		return tx.Get(&n, "SELECT count(*) FROM auditlog")
	}))
	assert.Zero(t, n, "operator actions are logged in the system tenant, not the organization")
}

func TestOperatorLimitsAndPasswordLength(t *testing.T) {
	d := pgtest.New(t)
	ctx := context.Background()
	o := NewOperator(d.App, &chconfig.Config{API: chconfig.APIConfig{PasswordMinLength: 40}})
	_, err := o.Create(ctx, ops, operator.NewOrganization{ID: "big", DeviceLimit: intp(1 << 40)})
	assert.ErrorIs(t, err, operator.ErrInvalid, "beyond a Postgres integer: 400, not 500")
	_, err = o.Create(ctx, ops, operator.NewOrganization{ID: "acme"})
	require.NoError(t, err)
	_, err = o.SetDeviceLimit(ctx, ops, "acme", intp(1<<31))
	assert.ErrorIs(t, err, operator.ErrInvalid)

	a, err := o.AddAdmin(ctx, ops, "acme", operator.NewAdmin{Username: "alice"})
	require.NoError(t, err, "the generated password meets a long password_min_length")
	assert.Len(t, a.Password, 40)
}

func TestOperatorWithoutGrantSaysHowToFix(t *testing.T) {
	o, d := newOperator(t)
	_, err := d.Owner.Exec("REVOKE EXECUTE ON FUNCTION rport_operator_add_tenant(text, text, integer) FROM " + pgtest.AppRole)
	require.NoError(t, err)
	_, err = o.Create(context.Background(), ops, operator.NewOrganization{ID: "acme"})
	assert.ErrorContains(t, err, "rportd migrate --grant-to")
}

// The device count of an organization follows the rule of its client limit: clients saved by a node with role all
// (clients) and clients only a device process reported (client_state) count, a client disconnected for longer than
// keep_disconnected_clients doesn't.
func TestOperatorDeviceCountCountsDeviceProcessClients(t *testing.T) {
	d := pgtest.New(t)
	cfg := &chconfig.Config{API: chconfig.APIConfig{PasswordMinLength: 14}}
	cfg.Server.PurgeDisconnectedClients = true
	cfg.Server.KeepDisconnectedClients = time.Hour
	o := NewOperator(d.App, cfg)
	ctx := context.Background()
	_, err := o.Create(ctx, ops, operator.NewOrganization{ID: "acme", DeviceLimit: intp(5)})
	require.NoError(t, err)
	_, err = d.Owner.Exec(`INSERT INTO clients (tenant_id, id, client_auth_id, details) VALUES ('acme', 'on-all-node', 'auth', '{}')`)
	require.NoError(t, err)
	reportState(t, d.Owner, "acme", "on-device-process", nil)
	old := time.Now().Add(-2 * time.Hour)
	reportState(t, d.Owner, "acme", "long-gone", &old)

	got, err := o.Get(ctx, "acme")
	require.NoError(t, err)
	assert.Equal(t, 2, got.DeviceCount)
	orgs, err := o.List(ctx)
	require.NoError(t, err)
	for _, org := range orgs {
		if org.ID == "acme" {
			assert.Equal(t, 2, org.DeviceCount)
		}
	}
	_, _, all := twoDeviceProcesses(t, d, &cfg.Server.KeepDisconnectedClients)
	assert.Equal(t, 3, admit(t, all, "acme", "a-new-one").Clients, "the admission counts the same (and the new one)")
}
