package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"
)

// DeviceFunctions are the functions the device database role may execute: lookups on the device path and the
// ingest functions (migration 1100), the session registry (1200) and the admission of devices (1350). migrate.GrantDevice grants EXECUTE on them; CheckDeviceRole requires it.
var DeviceFunctions = []string{
	"device_tenants()",
	"device_client_auth(text)",
	"device_client(text, text)",
	"device_client_groups(text)",
	"device_stored_tunnels(text, text)",
	// admission of devices across the cluster (migration 1350)
	"device_admit_client(text, text, timestamptz)",
	"ingest_job_results(jsonb)",
	"ingest_measurements(jsonb)",
	"ingest_connection_events(jsonb)",
	"ingest_audit(jsonb)",
	"report_client_state(jsonb)",
	// the session registry (migration 1200, migrate.RegistryFunctions)
	"rport_node_heartbeat(text, text, text)",
	// with the version, roles and process start (migration 1800; not SECURITY DEFINER, it calls the 3-argument form)
	"rport_node_heartbeat(text, text, text, text, text[], timestamptz)",
	"rport_register_session(text, text, text, text, integer)",
	"rport_renew_sessions(text, jsonb, integer)",
	"rport_end_session(text)",
}

// MeteringDeviceFunctions are the functions a device process with [metering] on also executes (migration 1300).
// migrate.GrantDevice grants them; CheckDeviceRole allows them, CheckDeviceMetering requires them. Kept apart from
// DeviceFunctions so a device process without metering starts on a role granted before migration 1300.
var MeteringDeviceFunctions = []string{"ingest_usage(jsonb)"}

// BillingDeviceFunctions are the functions a device process with [billing] on also executes (migration 1450): the
// transfer policies it throttles tunnels with. Granted by migrate.GrantDevice, allowed by CheckDeviceRole, required
// by CheckDeviceBilling.
var BillingDeviceFunctions = []string{"device_transfer_policy()"}

// CheckDeviceBilling returns an error unless the role db connects as may execute BillingDeviceFunctions.
func CheckDeviceBilling(ctx context.Context, db *sqlx.DB) error {
	return requireExecute(ctx, db, BillingDeviceFunctions)
}

// CheckDeviceMetering returns an error unless the role db connects as may execute MeteringDeviceFunctions.
func CheckDeviceMetering(ctx context.Context, db *sqlx.DB) error {
	return requireExecute(ctx, db, MeteringDeviceFunctions)
}

func requireExecute(ctx context.Context, db *sqlx.DB, fns []string) error {
	var name string
	if err := db.GetContext(ctx, &name, "SELECT current_user"); err != nil {
		return fmt.Errorf("pg: check role: %w", err)
	}
	for _, f := range fns {
		var ok bool
		if err := db.GetContext(ctx, &ok, "SELECT has_function_privilege(current_user, $1, 'EXECUTE')", f); err != nil {
			return fmt.Errorf("pg: check role: function %s: %w (run rportd migrate)", f, err)
		}
		if !ok {
			return fmt.Errorf("pg: role %q may not execute %s; grant it with rportd migrate --grant-device-to %s", name, f, name)
		}
	}
	return nil
}

// CheckDeviceRole returns an error unless the role db connects as has no more privilege than the device role
// needs (a node with [storage] role = "device"): no superuser, BYPASSRLS or table ownership (as CheckAppRole), no
// CREATEROLE or CREATEDB, no membership in any role, no privilege on any table, view, sequence or column, no CREATE
// on a schema, EXECUTE on every one of DeviceFunctions and on
// no other SECURITY DEFINER function. So a break-in through the device listener can't read accounts, tokens or the
// vault with the node's login.
func CheckDeviceRole(ctx context.Context, db *sqlx.DB) error {
	if err := CheckAppRole(ctx, db); err != nil {
		return err
	}
	var r struct {
		Name       string `db:"name"`
		CreateRole bool   `db:"createrole"`
		CreateDB   bool   `db:"createdb"`
		MemberOf   string `db:"member_of"`
	}
	if err := db.GetContext(ctx, &r, `SELECT current_user AS name, rolcreaterole AS createrole, rolcreatedb AS createdb,
		coalesce((SELECT string_agg(g.rolname, ', ' ORDER BY g.rolname) FROM pg_auth_members m
			JOIN pg_roles g ON g.oid = m.roleid WHERE m.member = r.oid), '') AS member_of
		FROM pg_roles r WHERE r.rolname = current_user`); err != nil {
		return fmt.Errorf("pg: check role: %w", err)
	}
	name := r.Name
	switch {
	case r.CreateRole, r.CreateDB:
		return fmt.Errorf("pg: role %q may create roles or databases, more than the device role needs", name)
	case r.MemberOf != "":
		// a member gets the rights of its roles (also the predefined pg_read_server_files and the like)
		return fmt.Errorf("pg: role %q is a member of %s, more than the device role needs", name, r.MemberOf)
	}
	var tables []string
	err := db.SelectContext(ctx, &tables, `SELECT n.nspname || '.' || c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'
			AND c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S')
			AND (CASE WHEN c.relkind = 'S' THEN has_sequence_privilege(current_user, c.oid, 'USAGE, SELECT, UPDATE')
				ELSE has_table_privilege(current_user, c.oid, 'SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER')
					OR has_any_column_privilege(current_user, c.oid, 'SELECT, INSERT, UPDATE, REFERENCES') END)
		ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("pg: check role: %w", err)
	}
	if len(tables) > 0 {
		return fmt.Errorf("pg: role %q has privileges on %d table(s), view(s) or sequence(s) (%s), more than the device role needs; connect as a role granted with rportd migrate --grant-device-to", name, len(tables), firstFew(tables))
	}
	var schemas []string
	if err := db.SelectContext(ctx, &schemas, `SELECT nspname FROM pg_namespace
		WHERE nspname NOT IN ('pg_catalog', 'information_schema') AND nspname NOT LIKE 'pg_toast%' AND nspname NOT LIKE 'pg_temp%'
			AND has_schema_privilege(current_user, oid, 'CREATE') ORDER BY 1`); err != nil {
		return fmt.Errorf("pg: check role: %w", err)
	}
	if len(schemas) > 0 {
		return fmt.Errorf("pg: role %q may create objects in schema(s) %s, more than the device role needs", name, firstFew(schemas))
	}
	if err := requireExecute(ctx, db, DeviceFunctions); err != nil {
		return err
	}
	allowed := append(append([]string(nil), DeviceFunctions...), existingFunctions(ctx, db, MeteringDeviceFunctions)...)
	allowed = append(allowed, existingFunctions(ctx, db, BillingDeviceFunctions)...)
	var extra []string
	if err := db.SelectContext(ctx, &extra, `SELECT p.oid::regprocedure::text
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE p.prosecdef AND n.nspname NOT IN ('pg_catalog', 'information_schema')
			AND p.prorettype <> 'trigger'::regtype AND has_function_privilege(current_user, p.oid, 'EXECUTE')
			AND p.oid <> ALL (SELECT unnest($1::text[])::regprocedure::oid)
		ORDER BY 1`, allowed); err != nil {
		return fmt.Errorf("pg: check role: %w", err)
	}
	if len(extra) > 0 {
		return fmt.Errorf("pg: role %q may execute SECURITY DEFINER function(s) the device role doesn't need: %s", name, firstFew(extra))
	}
	return nil
}

// existingFunctions returns those of fns that exist (a database migrated before they were added has not all).
func existingFunctions(ctx context.Context, db *sqlx.DB, fns []string) []string {
	var out []string
	for _, f := range fns {
		var ok bool
		if err := db.GetContext(ctx, &ok, "SELECT to_regprocedure($1) IS NOT NULL", f); err == nil && ok {
			out = append(out, f)
		}
	}
	return out
}

func firstFew(names []string) string {
	const n = 5
	if len(names) <= n {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:n], ", ") + ", …"
}
