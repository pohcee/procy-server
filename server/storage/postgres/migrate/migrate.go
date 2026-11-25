// Package migrate holds the server's Postgres schema as embedded SQL files and applies them.
//
// Files are named NNNN_name.up.sql (NNNN is the version, unique, applied in ascending order). Each domain owns a
// version range so independent changes don't collide: 0001–0099 core, 0100 users, tokens, sessions and client
// auth, 0200 clients, stored tunnels and client groups, 0300 jobs and schedules, 0400 library, notifications, audit
// log and monitoring, 1000 organizations (1000 the tenant of a client credential and device limits, 1050 the operator
// API). Only up migrations exist; a released file never changes.
package migrate

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/storage/postgres"
)

//go:embed migrations/*.sql
var files embed.FS

// lockKey is the advisory lock that serializes concurrent migration runs.
const lockKey = 7_301_220_001

// Migration is one schema change.
type Migration struct {
	Version int64
	Name    string
	SQL     string
}

// All returns every embedded migration in version order.
func All() ([]Migration, error) {
	entries, err := fs.ReadDir(files, "migrations")
	if err != nil {
		return nil, err
	}
	var out []Migration
	seen := map[int64]string{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			return nil, fmt.Errorf("migrate: unexpected file %q (want NNNN_name.up.sql)", name)
		}
		num, _, ok := strings.Cut(name, "_")
		if !ok {
			return nil, fmt.Errorf("migrate: file %q has no version prefix", name)
		}
		v, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migrate: file %q: bad version: %w", name, err)
		}
		if other, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrate: version %d used by %q and %q", v, other, name)
		}
		seen[v] = name
		b, err := fs.ReadFile(files, path.Join("migrations", name))
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: v, Name: name, SQL: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Up applies every migration that has not been applied yet, each in its own transaction under an advisory lock, so
// concurrent runs (several instances starting) are safe. It returns the versions it applied.
func Up(ctx context.Context, db *sqlx.DB) ([]int64, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	if err := inLock(ctx, db, func(tx *sqlx.Tx) error {
		// checked first, so a role without CREATE on the schema can run Up on a migrated database
		var exists bool
		if err := tx.GetContext(ctx, &exists, "SELECT to_regclass('schema_migrations') IS NOT NULL"); err != nil || exists {
			return err
		}
		_, err := tx.ExecContext(ctx, `CREATE TABLE schema_migrations (
			version bigint PRIMARY KEY,
			name text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now())`)
		return err
	}); err != nil {
		return nil, fmt.Errorf("migrate: create schema_migrations: %w", err)
	}
	var applied []int64
	for _, m := range all {
		m := m
		did := false
		err := inLock(ctx, db, func(tx *sqlx.Tx) error {
			var n int
			if err := tx.GetContext(ctx, &n, "SELECT count(*) FROM schema_migrations WHERE version = $1", m.Version); err != nil {
				return err
			}
			if n > 0 {
				return nil
			}
			if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, name) VALUES ($1, $2)", m.Version, m.Name); err != nil {
				return err
			}
			did = true
			return nil
		})
		if err != nil {
			return applied, fmt.Errorf("migrate: %s: %w", m.Name, err)
		}
		if did {
			applied = append(applied, m.Version)
		}
	}
	if err := runDeferred(ctx, db); err != nil {
		return applied, err
	}
	if err := resetFeedIfAhead(ctx, db); err != nil {
		return applied, err
	}
	return applied, nil
}

// runDeferred runs the deferred DDL (migrate_deferred, migration 0120) whose tables exist now.
func runDeferred(ctx context.Context, db *sqlx.DB) error {
	return inLock(ctx, db, func(tx *sqlx.Tx) error {
		var exists bool
		if err := tx.GetContext(ctx, &exists, "SELECT to_regclass('migrate_deferred') IS NOT NULL"); err != nil || !exists {
			return err
		}
		if _, err := tx.ExecContext(ctx, "SELECT rport_run_deferred()"); err != nil {
			return fmt.Errorf("migrate: deferred statements: %w", err)
		}
		return nil
	})
}

// resetFeedIfAhead empties the change feed when it holds transaction ids this database hasn't reached: it was
// copied from another Postgres cluster (pg_dump and restore), and would otherwise deliver nothing. Nodes must not be
// running; they take a fresh head when they start.
func resetFeedIfAhead(ctx context.Context, db *sqlx.DB) error {
	return inLock(ctx, db, func(tx *sqlx.Tx) error {
		var exists bool
		if err := tx.GetContext(ctx, &exists, "SELECT to_regclass('changes_horizon') IS NOT NULL"); err != nil || !exists {
			return err
		}
		var ahead bool
		if err := tx.GetContext(ctx, &ahead, `SELECT greatest((SELECT coalesce(max(tx_id), '0'::xid8) FROM changes), (SELECT tx_id FROM changes_horizon))
			>= pg_snapshot_xmax(pg_current_snapshot())`); err != nil {
			return fmt.Errorf("migrate: check the change feed: %w", err)
		}
		if !ahead {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "TRUNCATE changes"); err != nil {
			return fmt.Errorf("migrate: reset the change feed: %w", err)
		}
		_, err := tx.ExecContext(ctx, "UPDATE changes_horizon SET tx_id = '0', seq = 0")
		return err
	})
}

// Check returns an error unless every embedded migration has been applied.
func Check(ctx context.Context, db *sqlx.DB) error {
	all, err := All()
	if err != nil {
		return err
	}
	var have []int64
	if err := db.SelectContext(ctx, &have, "SELECT version FROM schema_migrations"); err != nil {
		return fmt.Errorf("migrate: database is not migrated (run \"rportd migrate\"): %w", err)
	}
	done := map[int64]bool{}
	for _, v := range have {
		done[v] = true
	}
	for _, m := range all {
		if !done[m.Version] {
			return fmt.Errorf("migrate: %s is not applied; run \"rportd migrate\"", m.Name)
		}
	}
	var pending []string
	if err := db.SelectContext(ctx, &pending, "SELECT name FROM migrate_deferred WHERE done_at IS NULL ORDER BY name"); err != nil {
		return fmt.Errorf("migrate: check deferred statements: %w", err)
	}
	if len(pending) > 0 {
		return fmt.Errorf("migrate: %s is not applied; run \"rportd migrate\"", pending[0])
	}
	return nil
}

// GrantApp gives role what the application needs, now and for tables later migrations add (default privileges
// of the role running it, which must be the schema owner): SELECT, INSERT, UPDATE and DELETE on the tenant-scoped
// tables, USAGE on sequences, SELECT only on tenants, user_logins, accounts, api_token_logins, client_auth_ids
// (written by triggers and the account functions), the change
// feed (changes, changes_horizon; written by triggers and rport_prune_changes), the cluster registry (nodes,
// client_sessions; written through RegistryFunctions), client_admissions (written by device_admit_client) and
// schema_migrations, and EXECUTE on the operator, registry and admission
// functions, which are the application's only way to change tenants and the registry. role must not own the tables
// and must not be a superuser or have
// BYPASSRLS, or row-level security does not hold for it (postgres.CheckAppRole).
func GrantApp(ctx context.Context, db *sqlx.DB, role string) error {
	q := quoteIdent(role)
	stmts := []string{
		"GRANT USAGE ON SCHEMA public TO " + q,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO " + q,
		"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO " + q,
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO " + q,
		"ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO " + q,
		"REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON schema_migrations, tenants, user_logins, client_auth_ids, changes, changes_horizon, migrate_deferred FROM " + q,
		// written only by rport_signup_delete_tenant (migration 1550)
		"REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON deleted_tenant_ids FROM " + q,
		"GRANT EXECUTE ON FUNCTION rport_prune_changes(interval) TO " + q,
		"REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON nodes, client_sessions, client_admissions FROM " + q,
		// nodes with role "all" admit devices like device processes do (migration 1350)
		"GRANT EXECUTE ON FUNCTION device_admit_client(text, text, timestamptz) TO " + q,
		// accounts and api_token_logins (migration 1400): written by triggers and the account functions
		"REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON accounts, api_token_logins FROM " + q,
	}
	for _, f := range RegistryFunctions {
		stmts = append(stmts, "GRANT EXECUTE ON FUNCTION "+f+" TO "+q)
	}
	// usage metering (migration 1300): nodes without a device process write their usage through ingest_usage,
	// and the operator API reads every tenant's usage through the summary functions
	for _, fn := range UsageFunctions {
		stmts = append(stmts, "GRANT EXECUTE ON FUNCTION "+fn+" TO "+q)
	}
	// billing (migration 1450): the billing tables are written only through its functions
	stmts = append(stmts, "REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON org_billing, billing_periods, billing_notices FROM "+q)
	for _, fn := range BillingFunctions {
		stmts = append(stmts, "GRANT EXECUTE ON FUNCTION "+fn+" TO "+q)
	}
	// the account functions (migration 1400) change the credential and remembered organization of a login
	for _, fn := range AccountFunctions {
		stmts = append(stmts, "GRANT EXECUTE ON FUNCTION "+fn+" TO "+q)
	}
	// signup (migration 1550): account status, email verification and the take-over of unproven accounts
	for _, fn := range SignupAccountFunctions {
		stmts = append(stmts, "GRANT EXECUTE ON FUNCTION "+fn+" TO "+q)
	}
	// the operator functions (migration 1050) change tenants for the operator API
	for _, fn := range OperatorFunctions {
		stmts = append(stmts, "GRANT EXECUTE ON FUNCTION "+fn+" TO "+q)
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("migrate: grant to %s: %w", role, err)
		}
	}
	return nil
}

// GrantDevice makes role the device role of a node with [storage] role = "device": it takes back every privilege
// role has on tables and sequences and grants USAGE on the schema and EXECUTE on postgres.DeviceFunctions only
// (lookups on the device path, the ingest functions, which check their input, and the session registry). role must
// be a separate login
// from the application role; postgres.CheckDeviceRole refuses anything more privileged.
func GrantDevice(ctx context.Context, db *sqlx.DB, role string) error {
	q := quoteIdent(role)
	stmts := []string{
		"REVOKE ALL ON ALL TABLES IN SCHEMA public FROM " + q,
		"REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM " + q,
		"GRANT USAGE ON SCHEMA public TO " + q,
	}
	fns := append(append([]string(nil), postgres.DeviceFunctions...), postgres.MeteringDeviceFunctions...)
	for _, f := range append(fns, postgres.BillingDeviceFunctions...) {
		stmts = append(stmts, "GRANT EXECUTE ON FUNCTION "+f+" TO "+q)
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("migrate: grant device role to %s: %w", role, err)
		}
	}
	return nil
}

// RegistryFunctions are the SECURITY DEFINER functions a device process records its node and its clients' sessions
// with (migration 1200). Only roles granted EXECUTE may call them: the application role (GrantApp, for nodes with
// storage role "all") and a device role.
var RegistryFunctions = []string{
	"rport_node_heartbeat(text, text, text)",
	"rport_register_session(text, text, text, text, integer)",
	"rport_renew_sessions(text, jsonb, integer)",
	"rport_end_session(text)",
}

// UsageFunctions are the SECURITY DEFINER functions of usage metering (migration 1300) the application role may
// execute; device roles get ingest_usage through postgres.DeviceFunctions.
var UsageFunctions = []string{
	"ingest_usage(jsonb)",
	"rport_usage_summary(timestamptz, timestamptz)",
	"rport_usage_device_summary(date, date)",
}

// BillingFunctions are the SECURITY DEFINER functions of billing (migration 1450) the application role may execute:
// the operator functions (which run only in the system tenant), the upkeep's, an administrator's spending limit and
// the transfer policies. Device roles get device_transfer_policy through postgres.BillingDeviceFunctions.
var BillingFunctions = []string{
	"rport_operator_set_plan(text, text, integer, text)",
	"rport_operator_set_billing(text, text, timestamptz, date, text, text, text, text)",
	"rport_operator_billing_override(text, timestamptz, bigint, date, boolean, text, text)",
	"rport_operator_approve_period(text, date, text)",
	"rport_operator_billing(text)",
	"rport_operator_billing_periods(text, text)",
	"rport_billing_ensure(text)",
	"rport_billing_transition(text, text, text, integer)",
	"rport_billing_set_policy(text, text, bigint, bigint, bigint, bigint, bigint, bigint, bigint, integer, text)",
	"rport_billing_close_period(jsonb)",
	"rport_billing_claim_notice(text, date, text)",
	"rport_billing_newest_usage()",
	"rport_billing_set_spending_limit(bigint, text)",
	"device_transfer_policy()",
}

// AccountFunctions are the SECURITY DEFINER functions through which the application changes accounts (migration
// 1400): the account of a login and the invitations of existing accounts.
var AccountFunctions = []string{
	"rport_set_account(text, text, boolean, text, text)",
	"rport_account_exists(text)",
	"rport_invite(text, text, text)",
	"rport_invitations_of(text)",
}

// SignupAccountFunctions are the SECURITY DEFINER functions of signup on accounts (migration 1550).
var SignupAccountFunctions = []string{
	"rport_account_status(text)",
	"rport_account_verify_email(text)",
	"rport_account_take_over(text, text, text)",
}

// OperatorFunctions are the SECURITY DEFINER functions through which platform operators change tenants.
var OperatorFunctions = []string{
	"rport_operator_add_tenant(text, text, integer)",
	"rport_operator_set_tenant_status(text, text)",
	"rport_operator_set_max_clients(text, integer)",
	// self-service signup (migration 1550): deleting an organization whose deletion is due
	"rport_signup_delete_tenant(text)",
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func inLock(ctx context.Context, db *sqlx.DB, fn func(tx *sqlx.Tx) error) (err error) {
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", int64(lockKey)); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
