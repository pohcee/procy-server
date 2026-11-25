package postgres_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/migrate"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
)

const fakeHash = "$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2FsdA$aGFzaGhhc2hoYXNoaGFzaGhhc2hoYXNoaGFzaGhhc2g"

func isPermissionDenied(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "42501"
}

// The device role reads no table at all, so not users, groups, tokens, sessions, the vault or wrapped keys, and
// writes none, not even the device-path ones.
func TestDeviceRoleHasNoTableAccess(t *testing.T) {
	d := pgtest.New(t, "a")
	ctx := context.Background()
	for _, tbl := range []string{"users", "user_groups", "groups", "api_tokens", "api_sessions", "vault_values",
		"vault_status", "vault_wrapped_keys", "clients_auth", "clients", "client_state", "client_records",
		"client_groups", "stored_tunnels", "jobs", "measurements", "auditlog", "ingest_keys", "tenants", "user_logins"} {
		_, err := d.Device.ExecContext(ctx, "SELECT 1 FROM "+tbl+" LIMIT 1")
		assert.True(t, isPermissionDenied(err), "SELECT %s: %v", tbl, err)
	}
	_, err := d.Device.ExecContext(ctx, "INSERT INTO client_state (tenant_id, client_id, client_auth_id, details, reported_at) VALUES ('a', 'c', 'x', '{}', now())")
	assert.True(t, isPermissionDenied(err), "%v", err)
	_, err = d.Device.ExecContext(ctx, `SELECT rport_ingest_claim('{"tenant_id":"a","key":"k"}', 'audit')`)
	assert.True(t, isPermissionDenied(err), "%v", err)
	_, err = d.Device.ExecContext(ctx, "CREATE TABLE mine (x int)")
	assert.True(t, isPermissionDenied(err), "%v", err)
}

func TestCheckDeviceRole(t *testing.T) {
	d := pgtest.New(t)
	ctx := context.Background()
	assert.NoError(t, postgres.CheckDeviceRole(ctx, d.Device))
	assert.ErrorContains(t, postgres.CheckDeviceRole(ctx, d.App), "more than the device role needs")
	assert.ErrorContains(t, postgres.CheckDeviceRole(ctx, d.Owner), "row-level security")

	// a device login also granted one table is refused
	_, err := d.Owner.ExecContext(ctx, "GRANT SELECT ON users TO "+pgtest.DeviceRole)
	require.NoError(t, err)
	assert.ErrorContains(t, postgres.CheckDeviceRole(ctx, d.Device), "public.users")
	require.NoError(t, migrate.GrantDevice(ctx, d.Owner, pgtest.DeviceRole), "GrantDevice takes table privileges back")
	assert.NoError(t, postgres.CheckDeviceRole(ctx, d.Device))

	// a device login that may run another SECURITY DEFINER function is refused
	_, err = d.Owner.ExecContext(ctx, "GRANT EXECUTE ON FUNCTION rport_ingest_claim(jsonb, text) TO "+pgtest.DeviceRole)
	require.NoError(t, err)
	assert.ErrorContains(t, postgres.CheckDeviceRole(ctx, d.Device), "rport_ingest_claim")
	_, err = d.Owner.ExecContext(ctx, "REVOKE EXECUTE ON FUNCTION rport_ingest_claim(jsonb, text) FROM "+pgtest.DeviceRole)
	require.NoError(t, err)

	// a role granted before usage metering (migration 1300) still starts a device process without metering
	assert.NoError(t, postgres.CheckDeviceMetering(ctx, d.Device))
	_, err = d.Owner.ExecContext(ctx, "REVOKE EXECUTE ON FUNCTION ingest_usage(jsonb) FROM "+pgtest.DeviceRole)
	require.NoError(t, err)
	assert.NoError(t, postgres.CheckDeviceRole(ctx, d.Device))
	assert.ErrorContains(t, postgres.CheckDeviceMetering(ctx, d.Device), "ingest_usage")

	// likewise before billing (migration 1450): device_transfer_policy is needed only with [billing] on
	assert.NoError(t, postgres.CheckDeviceBilling(ctx, d.Device))
	_, err = d.Owner.ExecContext(ctx, "REVOKE EXECUTE ON FUNCTION device_transfer_policy() FROM "+pgtest.DeviceRole)
	require.NoError(t, err)
	assert.NoError(t, postgres.CheckDeviceRole(ctx, d.Device))
	assert.ErrorContains(t, postgres.CheckDeviceBilling(ctx, d.Device), "device_transfer_policy")
	// the other billing functions are not for the device role
	_, err = d.Owner.ExecContext(ctx, "GRANT EXECUTE ON FUNCTION rport_billing_set_policy(text, text, bigint, bigint, bigint, bigint, bigint, bigint, bigint, integer, text) TO "+pgtest.DeviceRole)
	require.NoError(t, err)
	assert.ErrorContains(t, postgres.CheckDeviceRole(ctx, d.Device), "rport_billing_set_policy")
	_, err = d.Owner.ExecContext(ctx, "REVOKE EXECUTE ON FUNCTION rport_billing_set_policy(text, text, bigint, bigint, bigint, bigint, bigint, bigint, bigint, integer, text) FROM "+pgtest.DeviceRole)
	require.NoError(t, err)

	// one without the grants is told to run the grant
	_, err = d.Owner.ExecContext(ctx, "REVOKE EXECUTE ON FUNCTION ingest_audit(jsonb) FROM "+pgtest.DeviceRole)
	require.NoError(t, err)
	assert.ErrorContains(t, postgres.CheckDeviceRole(ctx, d.Device), "--grant-device-to")
}

func TestDeviceLookups(t *testing.T) {
	d := pgtest.New(t, "a", "b")
	ctx := context.Background()
	exec(t, d.Owner, "INSERT INTO clients_auth (tenant_id, id, password) VALUES ('a', 'hashed', $1), ('b', 'plain', 'secret')", fakeHash)
	exec(t, d.Owner, `INSERT INTO clients (tenant_id, id, client_auth_id, details) VALUES ('a', 'c1', 'hashed', '{"name":"admin","allowed_user_groups":["ops"]}')`)
	exec(t, d.Owner, `INSERT INTO client_groups (tenant_id, id, description, params) VALUES ('a', 'g1', 'G', '{}'), ('b', 'g2', 'G', '{}')`)

	var auth []struct {
		TenantID string `db:"tenant_id"`
		ID       string `db:"id"`
		Hash     string `db:"password_hash"`
	}
	require.NoError(t, d.Device.SelectContext(ctx, &auth, "SELECT * FROM device_client_auth('hashed')"))
	require.Len(t, auth, 1, "found across tenants")
	assert.Equal(t, "a", auth[0].TenantID)
	assert.Equal(t, fakeHash, auth[0].Hash)
	auth = nil
	require.NoError(t, d.Device.SelectContext(ctx, &auth, "SELECT * FROM device_client_auth('plain')"))
	assert.Empty(t, auth, "a password not stored as a hash is never returned")

	var groups []string
	require.NoError(t, d.Device.SelectContext(ctx, &groups, "SELECT id FROM device_client_groups('a')"))
	assert.Equal(t, []string{"g1"}, groups)

	var details string
	require.NoError(t, d.Device.GetContext(ctx, &details, "SELECT details FROM device_client('a', 'c1')"))
	assert.Contains(t, details, `"admin"`)

	// the app role sees nothing of the cross-tenant policy by setting it
	var n int
	require.NoError(t, postgres.InTenantID(ctx, d.App, "b", func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, "SELECT set_config('rport.cross_tenant_read', 'on', true)"); err != nil {
			return err
		}
		return tx.GetContext(ctx, &n, "SELECT count(*) FROM clients_auth")
	}))
	assert.Equal(t, 1, n)
}

type ingestResult struct {
	Key    string `db:"out_key"`
	Status string `db:"out_status"`
}

func ingest(t *testing.T, db *sqlx.DB, fn string, items ...map[string]any) []ingestResult {
	t.Helper()
	b, err := json.Marshal(items)
	require.NoError(t, err)
	var res []ingestResult
	require.NoError(t, db.Select(&res, "SELECT out_key, out_status FROM "+fn+"($1::jsonb)", string(b)))
	return res
}

func statuses(rs []ingestResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Status
		if strings.HasPrefix(r.Status, "rejected") {
			out[i] = "rejected"
		}
	}
	return out
}

func exec(t *testing.T, db *sqlx.DB, q string, args ...any) {
	t.Helper()
	_, err := db.Exec(q, args...)
	require.NoError(t, err, q)
}

func count(t *testing.T, db *sqlx.DB, q string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.Get(&n, q, args...), q)
	return n
}

var t0 = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

func TestIngestIdempotent(t *testing.T) {
	d := pgtest.New(t, "a", "b")
	exec(t, d.Owner, "INSERT INTO tenants (id, status) VALUES ('s', 'suspended')")
	exec(t, d.Owner, "INSERT INTO clients_auth (tenant_id, id, password) VALUES ('a', 'auth-a', $1)", fakeHash)

	job := map[string]any{"tenant_id": "a", "key": "j1", "jid": "jid-1", "client_id": "c1", "status": "successful",
		"started_at": t0, "finished_at": t0.Add(time.Second), "created_by": "admin", "details": `{"result":{"stdout":"hi"}}`}
	assert.Equal(t, []string{"applied"}, statuses(ingest(t, d.Device, "ingest_job_results", job)))
	assert.Equal(t, []string{"duplicate"}, statuses(ingest(t, d.Device, "ingest_job_results", job)))
	assert.Equal(t, 1, count(t, d.Owner, "SELECT count(*) FROM jobs WHERE tenant_id = 'a' AND jid = 'jid-1' AND status = 'successful'"))

	m := map[string]any{"tenant_id": "a", "key": "m1", "client_id": "c1", "timestamp": t0, "cpu_usage_percent": 1.5,
		"memory_usage_percent": 2, "io_usage_percent": 3, "processes": `[]`, "net_lan_in": 7}
	assert.Equal(t, []string{"applied"}, statuses(ingest(t, d.Device, "ingest_measurements", m)))
	m["key"] = "m1-again"
	assert.Equal(t, []string{"duplicate"}, statuses(ingest(t, d.Device, "ingest_measurements", m)), "one per client and time")

	ev := map[string]any{"tenant_id": "a", "key": "e1", "client_id": "c1", "client_auth_id": "auth-a", "event": "connected", "at": t0, "node_id": "n1"}
	assert.Equal(t, []string{"applied", "duplicate"}, statuses(ingest(t, d.Device, "ingest_connection_events", ev, ev)))
	assert.Equal(t, 1, count(t, d.Owner, "SELECT count(*) FROM client_connection_events"))

	au := map[string]any{"tenant_id": "a", "key": "a1", "timestamp": t0, "application": "client.command", "action": "execute.done", "client_id": "c1"}
	assert.Equal(t, []string{"applied"}, statuses(ingest(t, d.Device, "ingest_audit", au)))
	assert.Equal(t, []string{"duplicate"}, statuses(ingest(t, d.Device, "ingest_audit", au)))
	assert.Equal(t, 1, count(t, d.Owner, "SELECT count(*) FROM auditlog"))

	st := map[string]any{"tenant_id": "a", "key": "s1", "client_id": "c1", "client_auth_id": "auth-a", "reported_at": t0,
		"details": `{"name":"dev","allowed_user_groups":["evil"]}`}
	assert.Equal(t, []string{"applied", "duplicate"}, statuses(ingest(t, d.Device, "report_client_state", st, st)))
	older := map[string]any{"tenant_id": "a", "key": "s0", "client_id": "c1", "client_auth_id": "auth-a", "reported_at": t0.Add(-time.Minute), "details": `{"name":"old"}`}
	assert.Equal(t, []string{"duplicate"}, statuses(ingest(t, d.Device, "report_client_state", older)), "an older report doesn't win")
	var details string
	require.NoError(t, d.Owner.Get(&details, "SELECT details::text FROM client_state"))
	assert.Equal(t, `{"name": "dev"}`, details, "allowed_user_groups is never taken from a device")

}

func TestIngestRejectsInvalidItems(t *testing.T) {
	d := pgtest.New(t, "a", "b")
	exec(t, d.Owner, "INSERT INTO tenants (id, status) VALUES ('s', 'suspended')")
	exec(t, d.Owner, "INSERT INTO clients_auth (tenant_id, id, password) VALUES ('a', 'auth-a', $1)", fakeHash)
	exec(t, d.Owner, `INSERT INTO jobs (tenant_id, jid, status, started_at, created_by, client_id, details) VALUES ('a', 'other', 'running', now(), 'x', 'c2', '{}')`)

	base := func(kv ...any) map[string]any {
		m := map[string]any{"tenant_id": "a", "key": "k", "jid": "j", "client_id": "c1", "status": "running",
			"started_at": t0, "created_by": "admin", "details": `{}`}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	res := ingest(t, d.Device, "ingest_job_results",
		base("tenant_id", "s", "key", "1"),
		base("tenant_id", "nope", "key", "2"),
		base("key", ""),
		base("key", "4", "status", "pwned"),
		base("key", "5", "details", "not json"),
		base("key", "6", "jid", "other"),
		base("key", "7", "multi_job_id", "missing"),
		base("key", "8", "started_at", "not a time"),
		base("key", "9"),
	)
	assert.Equal(t, []string{"rejected", "rejected", "rejected", "rejected", "rejected", "rejected", "rejected", "rejected", "applied"}, statuses(res))
	assert.Contains(t, res[5].Status, "another client")
	assert.Equal(t, 0, count(t, d.Owner, "SELECT count(*) FROM jobs WHERE client_id = 'c1' AND jid <> 'j'"))

	// a state report naming a credential of another tenant is rejected
	exec(t, d.Owner, "INSERT INTO clients_auth (tenant_id, id, password) VALUES ('b', 'auth-b', $1)", fakeHash)
	res = ingest(t, d.Device, "report_client_state",
		map[string]any{"tenant_id": "a", "key": "x", "client_id": "c1", "client_auth_id": "auth-b", "reported_at": t0, "details": `{}`},
		map[string]any{"tenant_id": "a", "key": "y", "client_id": "c1", "client_auth_id": "auth-a", "reported_at": t0, "details": `[1]`},
		map[string]any{"tenant_id": "a", "key": "z", "client_id": "c1", "client_auth_id": "auth-a", "details": `{}`},
	)
	assert.Equal(t, []string{"rejected", "rejected", "rejected"}, statuses(res))

	// whole batches that are not arrays, or too big, fail
	_, err := d.Device.Exec("SELECT * FROM ingest_audit('{}'::jsonb)")
	assert.ErrorContains(t, err, "JSON array")
	big := make([]map[string]any, 1001)
	for i := range big {
		big[i] = map[string]any{}
	}
	b, _ := json.Marshal(big)
	_, err = d.Device.Exec("SELECT * FROM ingest_audit($1::jsonb)", string(b))
	assert.ErrorContains(t, err, "at most 1000")
}

// The API reads clients through client_records: a device-reported state wins except for the admin-set
// allowed_user_groups, which come only from clients (empty without a clients row).
func TestClientRecordsView(t *testing.T) {
	d := pgtest.New(t, "a")
	exec(t, d.Owner, "INSERT INTO clients_auth (tenant_id, id, password) VALUES ('a', 'auth-a', $1)", fakeHash)
	exec(t, d.Owner, `INSERT INTO clients (tenant_id, id, client_auth_id, details) VALUES
		('a', 'admin-only', 'auth-a', '{"name":"stored","allowed_user_groups":["ops"]}'),
		('a', 'both', 'auth-a', '{"name":"stale","allowed_user_groups":["ops"]}')`)
	for _, id := range []string{"both", "state-only"} {
		ingest(t, d.Device, "report_client_state", map[string]any{"tenant_id": "a", "key": id, "client_id": id,
			"client_auth_id": "auth-a", "reported_at": t0, "disconnected_at": t0, "details": `{"name":"fresh","allowed_user_groups":["evil"]}`})
	}
	type rec struct {
		ID             string     `db:"id"`
		DisconnectedAt *time.Time `db:"disconnected_at"`
		Details        string     `db:"details"`
	}
	var rows []rec
	require.NoError(t, postgres.InTenantID(context.Background(), d.App, "a", func(tx *sqlx.Tx) error {
		return tx.Select(&rows, "SELECT id, disconnected_at, details FROM client_records ORDER BY id")
	}))
	require.Len(t, rows, 3)
	assert.Equal(t, "admin-only", rows[0].ID)
	assert.JSONEq(t, `{"name":"stored","allowed_user_groups":["ops"]}`, rows[0].Details)
	assert.Nil(t, rows[0].DisconnectedAt)
	assert.Equal(t, "both", rows[1].ID)
	assert.JSONEq(t, `{"name":"fresh","allowed_user_groups":["ops"]}`, rows[1].Details)
	assert.NotNil(t, rows[1].DisconnectedAt)
	assert.Equal(t, "state-only", rows[2].ID)
	assert.JSONEq(t, `{"name":"fresh","allowed_user_groups":[]}`, rows[2].Details)
}

// With a schema owner that is not a superuser, row-level security is forced on the owner too: the device functions
// must still read across tenants and write into the item's tenant.
func TestDeviceFunctionsNonSuperuserOwner(t *testing.T) {
	ctx := context.Background()
	d := pgtest.NewEmpty(t)
	suffix := strings.TrimPrefix(d.Name, "t_")
	owner, device := "own_"+suffix, "dev_"+suffix
	for _, stmt := range []string{
		"CREATE ROLE " + owner + " LOGIN PASSWORD 'pw' NOSUPERUSER NOBYPASSRLS",
		"CREATE ROLE " + device + " LOGIN PASSWORD 'pw' NOSUPERUSER NOBYPASSRLS",
		"ALTER DATABASE " + d.Name + " OWNER TO " + owner,
		"ALTER SCHEMA public OWNER TO " + owner,
	} {
		_, err := d.Owner.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = d.Owner.ExecContext(bg, "ALTER DATABASE "+d.Name+" OWNER TO CURRENT_USER")
		_, _ = d.Owner.ExecContext(bg, "REASSIGN OWNED BY "+owner+" TO CURRENT_USER")
		_, _ = d.Owner.ExecContext(bg, "DROP OWNED BY "+owner+", "+device+" CASCADE")
		for _, r := range []string{device, owner} {
			_, _ = d.Owner.ExecContext(bg, "DROP ROLE IF EXISTS "+r)
		}
	})
	ownerDB := openAs(t, d.OwnerURL, owner)
	_, err := migrate.Up(ctx, ownerDB)
	require.NoError(t, err)
	require.NoError(t, migrate.GrantDevice(ctx, ownerDB, device))
	exec(t, d.Owner, "INSERT INTO tenants (id) VALUES ('a')")
	exec(t, d.Owner, "INSERT INTO clients_auth (tenant_id, id, password) VALUES ('a', 'auth-a', $1)", fakeHash)

	devDB := openAs(t, d.OwnerURL, device)
	require.NoError(t, postgres.CheckDeviceRole(ctx, devDB))
	var tid string
	require.NoError(t, devDB.Get(&tid, "SELECT tenant_id FROM device_client_auth('auth-a')"))
	assert.Equal(t, "a", tid)
	res := ingest(t, devDB, "report_client_state", map[string]any{"tenant_id": "a", "key": "s", "client_id": "c1",
		"client_auth_id": "auth-a", "reported_at": t0, "details": `{"name":"x"}`})
	assert.Equal(t, []string{"applied"}, statuses(res))
	var n int
	require.NoError(t, devDB.Get(&n, "SELECT count(*) FROM device_client('a', 'c1')"))
	assert.Equal(t, 1, n)
	// the admission counts the tenant's clients under row-level security
	exec(t, d.Owner, "UPDATE tenants SET max_clients = 1 WHERE id = 'a'")
	var decision string
	require.NoError(t, devDB.Get(&decision, "SELECT decision FROM device_admit_client('a', 'c1', NULL)"))
	assert.Equal(t, "admitted", decision, "c1 is the tenant's client")
	require.NoError(t, devDB.Get(&decision, "SELECT decision FROM device_admit_client('a', 'c2', NULL)"))
	assert.Equal(t, "refused_limit", decision)
}

func TestIngestReviewCases(t *testing.T) {
	d := pgtest.New(t, "a")
	// an indexed value too long for its index (random, so it doesn't compress) is rejected, not an error for the
	// whole batch
	raw := make([]byte, 6000)
	_, _ = rand.Read(raw)
	res := ingest(t, d.Device, "ingest_audit",
		map[string]any{"tenant_id": "a", "key": "big", "timestamp": t0, "application": "x", "action": "y", "client_hostname": hex.EncodeToString(raw)},
		map[string]any{"tenant_id": "a", "key": "ok", "timestamp": t0, "application": "x", "action": "y"})
	assert.Equal(t, []string{"rejected", "applied"}, statuses(res))

	// a connection event without node_id
	exec(t, d.Owner, "INSERT INTO clients_auth (tenant_id, id, password) VALUES ('a', 'auth-a', $1)", fakeHash)
	res = ingest(t, d.Device, "ingest_connection_events", map[string]any{"tenant_id": "a", "key": "e", "client_id": "c1", "client_auth_id": "auth-a", "event": "connected", "at": t0})
	assert.Equal(t, []string{"applied"}, statuses(res))

	// library fields: a result without them keeps the stored ones; a create after a result keeps the result
	exec(t, d.Owner, `INSERT INTO jobs (tenant_id, jid, status, started_at, created_by, client_id, details)
		VALUES ('a', 'lib', 'running', now(), 'x', 'c1', '{"command":"x","library_entry_id":"e1","library_version":3,"os":"linux"}')`)
	res = ingest(t, d.Device, "ingest_job_results", map[string]any{"tenant_id": "a", "key": "r", "jid": "lib", "client_id": "c1",
		"status": "successful", "started_at": t0, "finished_at": t0, "created_by": "x", "details": `{"command":"x","result":{"stdout":"done"}}`})
	assert.Equal(t, []string{"applied"}, statuses(res))
	var details string
	require.NoError(t, d.Owner.Get(&details, "SELECT details FROM jobs WHERE jid = 'lib'"))
	assert.JSONEq(t, `{"command":"x","result":{"stdout":"done"},"library_entry_id":"e1","library_version":3,"os":"linux"}`, details)

	res = ingest(t, d.Device, "ingest_job_results",
		map[string]any{"tenant_id": "a", "key": "fast", "jid": "j2", "client_id": "c1", "status": "successful", "started_at": t0, "finished_at": t0, "created_by": "x", "details": `{"result":{"stdout":"fast"}}`},
		map[string]any{"tenant_id": "a", "key": "create", "jid": "j2", "client_id": "c1", "status": "running", "started_at": t0, "created_by": "x", "create_only": true, "details": `{"library_entry_id":"e2","library_version":1}`})
	assert.Equal(t, []string{"applied", "applied"}, statuses(res))
	var status string
	require.NoError(t, d.Owner.Get(&status, "SELECT status FROM jobs WHERE jid = 'j2'"))
	assert.Equal(t, "successful", status, "the later create keeps the result")
	require.NoError(t, d.Owner.Get(&details, "SELECT details FROM jobs WHERE jid = 'j2'"))
	assert.JSONEq(t, `{"result":{"stdout":"fast"},"library_entry_id":"e2","library_version":1}`, details, "and adds its library fields")
}

// Lookups by tenant and id use the primary keys, not a scan of every tenant's clients.
func TestClientRecordsUsesIndexes(t *testing.T) {
	d := pgtest.New(t, "a")
	exec(t, d.Owner, "SET enable_seqscan = off")
	var plan []string
	require.NoError(t, d.Owner.Select(&plan, "EXPLAIN SELECT * FROM client_records WHERE tenant_id = 'a' AND id = 'x'"))
	joined := strings.Join(plan, "\n")
	assert.NotContains(t, joined, "Seq Scan", joined)
	assert.NotContains(t, joined, "Full Join", joined)
}

func TestCheckDeviceRoleRefusesMembership(t *testing.T) {
	d := pgtest.New(t)
	ctx := context.Background()
	exec(t, d.Owner, "GRANT pg_read_server_files TO "+pgtest.DeviceRole)
	t.Cleanup(func() { _, _ = d.Owner.Exec("REVOKE pg_read_server_files FROM " + pgtest.DeviceRole) })
	assert.ErrorContains(t, postgres.CheckDeviceRole(ctx, d.Device), "member of pg_read_server_files")
}
