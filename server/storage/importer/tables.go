package importer

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openrport/openrport/server/clientsauth"
)

// Entity names in the report.
const (
	entClientAuth       = "client auth"
	entClients          = "clients"
	entStoredTunnels    = "stored tunnels"
	entClientGroups     = "client groups"
	entUsers            = "users"
	entUserGroups       = "user group memberships"
	entGroupPermissions = "user group permissions"
	entAPITokens        = "api tokens"
	entAPISessions      = "api sessions"
	entLibraryEntries   = "library entries"
	entLibraryVersions  = "library versions"
	entLibraryMeta      = "library meta"
	entSchedules        = "schedules"
	entVaultValues      = "vault values"
	entVaultStatus      = "vault status"
	entMultiJobs        = "multi jobs"
	entJobs             = "jobs"
	entNotifications    = "notifications"
	entAuditLog         = "audit log"
	entMonitoring       = "monitoring"
)

const onConflictNothing = "ON CONFLICT DO NOTHING"

// copyTable copies a source table whose rows the target identifies by its key: rows already there are present.
func (im *importer) copyTable(w *writer, ent, file, table string, src []col, dst []string, orderBy string) error {
	count := im.count(ent)
	d := im.dbs[file]
	if d == nil {
		return nil
	}
	if ok, err := hasTable(d.db, table); err != nil || !ok {
		return err
	}
	b := w.batch(pgTable(table), dst, onConflictNothing, count)
	err := readRows(d, table, src, orderBy, func(vals []any) error {
		count.Read++
		return b.add(vals)
	})
	if err != nil {
		return err
	}
	return b.flush()
}

// pgTable maps a SQLite table name onto its Postgres table.
func pgTable(sqliteTable string) string {
	switch sqliteTable {
	case "entries":
		return "library_entries"
	case "entry_versions":
		return "library_entry_versions"
	case "values":
		return "vault_values"
	case "status":
		return "vault_status"
	}
	return sqliteTable
}

func names(cols []col) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.src
	}
	return out
}

// --- client credentials ------------------------------------------------------

// writeClientAuth copies the client credentials with their ids and the argon2id hashes of their passwords (a
// standalone server compares the stored password as it is, so whatever is stored is the password, even one that
// looks like a hash). A client auth id is unique across tenants (a connecting client doesn't name its tenant): one
// another tenant uses is skipped.
func (im *importer) writeClientAuth(w *writer) error {
	if im.auth == nil {
		return nil
	}
	count := im.count(entClientAuth)
	var taken []string
	for _, cr := range im.creds {
		count.Read++
		hash, err := clientsauth.HashPassword(cr.Password)
		if err != nil {
			return err
		}
		added, err := w.insertOne("clients_auth", []string{"id", "password"}, []any{cr.ID, hash})
		if err != nil {
			return err
		}
		if added {
			count.Added++
			continue
		}
		here, err := w.exists("SELECT 1 FROM clients_auth WHERE tenant_id = $1 AND id = $2", cr.ID)
		if err != nil {
			return err
		}
		if here {
			count.Present++
		} else {
			count.Skipped++
			taken = append(taken, cr.ID)
		}
	}
	if len(taken) > 0 {
		im.rep.warnf("client auth: ids used by another tenant, not imported: %s", strings.Join(taken, ", "))
	}
	return nil
}

// --- clients -----------------------------------------------------------------

var (
	clientCols = []col{c("id", kText), c("client_auth_id", kText), c("disconnected_at", kNTime), c("details", kText)}
	tunnelCols = []col{
		c("id", kText), c("client_id", kText), c("created_at", kNTime), c("name", kNText), c("scheme", kNText),
		c("remote_ip", kNText), c("remote_port", kNInt), c("acl", kNText), c("public_port", kNInt), c("further_options", kNText),
	}
)

func (im *importer) writeClients(w *writer) error {
	if err := im.copyTable(w, entClients, fileClients, "clients", clientCols, names(clientCols), "id"); err != nil {
		return err
	}
	// stored tunnels need their client (a foreign key Postgres enforces and the SQLite file never did)
	count := im.count(entStoredTunnels)
	d := im.dbs[fileClients]
	if d == nil {
		return nil
	}
	b := w.batch("stored_tunnels", names(tunnelCols), onConflictNothing, count)
	orphans := 0
	err := readRows(d, "stored_tunnels", tunnelCols, "client_id, id", func(vals []any) error {
		count.Read++
		ok, err := w.exists("SELECT 1 FROM clients WHERE tenant_id = $1 AND id = $2", vals[1])
		if err != nil {
			return err
		}
		if !ok {
			count.Skipped++
			orphans++
			return nil
		}
		return b.add(vals)
	})
	if err != nil {
		return err
	}
	if orphans > 0 {
		im.rep.warnf("stored tunnels: %d of clients that don't exist, not imported", orphans)
	}
	return b.flush()
}

func (im *importer) writeClientGroups(w *writer) error {
	cols := []col{c("id", kText), c("description", kText), c("params", kText), c("allowed_user_groups", kText)}
	return im.copyTable(w, entClientGroups, fileClientGroups, "client_groups", cols, names(cols), "id")
}

// --- users -------------------------------------------------------------------

// writeUsers copies the API users (password hashes as stored), their group memberships and the stored group
// permissions. A username is unique across tenants: one another tenant uses is skipped.
func (im *importer) writeUsers(w *writer) error {
	if im.auth == nil {
		return nil
	}
	uc, mc, gc := im.count(entUsers), im.count(entUserGroups), im.count(entGroupPermissions)
	var taken []string
	for _, u := range im.users {
		uc.Read++
		mc.Read += len(u.Groups)
		// a login of other tenants only is taken: the importer doesn't make memberships of existing accounts
		var takenElsewhere bool
		err := w.tx.GetContext(w.ctx, &takenElsewhere, `SELECT EXISTS (SELECT 1 FROM user_logins WHERE username = $1 AND tenant_id <> $2)
			AND NOT EXISTS (SELECT 1 FROM user_logins WHERE username = $1 AND tenant_id = $2)`, u.Username, string(w.tid))
		if err != nil {
			return err
		}
		if takenElsewhere {
			uc.Skipped++
			mc.Skipped += len(u.Groups)
			taken = append(taken, u.Username)
			continue
		}
		added, err := w.insertOne("users", []string{"username", "password", "password_expired", "two_fa_send_to", "totp_secret"},
			[]any{u.Username, u.Password, u.PasswordExpired, u.TwoFASendTo, u.TotPSecret})
		if err != nil {
			return err
		}
		if added {
			uc.Added++
		} else {
			uc.Present++
		}
		for _, g := range u.Groups {
			added, err := w.insertOne("user_groups", []string{"username", "group_name"}, []any{u.Username, g})
			if err != nil {
				return err
			}
			if added {
				mc.Added++
			} else {
				mc.Present++
			}
		}
	}
	if len(taken) > 0 {
		im.rep.warnf("users: usernames used by another tenant, not imported: %s", strings.Join(taken, ", "))
	}
	for _, g := range im.groups {
		gc.Read++
		added, err := w.insertOne("groups", []string{"name", "permissions", "tunnels_restricted", "commands_restricted"},
			[]any{g.Name, g.Permissions, g.TunnelsRestricted, g.CommandsRestricted})
		if err != nil {
			return err
		}
		if added {
			gc.Added++
		} else {
			gc.Present++
		}
	}
	return nil
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// --- API tokens and sessions -------------------------------------------------

// writeAPITokens copies the API tokens of the users the target tenant has once the users are imported (imported or
// already there). A token of any other user is skipped: it would belong to nobody, or to a login of another tenant.
func (im *importer) writeAPITokens(w *writer) error {
	count := im.count(entAPITokens)
	d := im.dbs[fileAPITokens]
	if d == nil {
		return nil
	}
	if ok, err := hasTable(d.db, "api_tokens"); err != nil || !ok {
		return err
	}
	cols := []col{
		c("username", kText), c("prefix", kText), c("created_at", kTime), c("expires_at", kNTime), c("scope", kNText),
		c("token", kText), c("name", kText),
	}
	b := w.batch("api_tokens", names(cols), onConflictNothing, count)
	orphans := 0
	err := readRows(d, "api_tokens", cols, "username, prefix", func(vals []any) error {
		count.Read++
		ok, err := im.targetHasUser(w, vals[0])
		if err != nil {
			return err
		}
		if !ok {
			count.Skipped++
			orphans++
			return nil
		}
		return b.add(vals)
	})
	if err != nil {
		return err
	}
	if orphans > 0 {
		im.rep.warnf("api tokens: %d tokens of users the tenant doesn't have, not imported", orphans)
	}
	return b.flush()
}

// targetHasUser reports whether the target tenant has the user named username (imported or already there).
func (im *importer) targetHasUser(w *writer, username any) (bool, error) {
	name := fmt.Sprint(username)
	if ok, seen := im.targetUsers[name]; seen {
		return ok, nil
	}
	ok, err := w.exists("SELECT 1 FROM users WHERE tenant_id = $1 AND username = $2", name)
	if err != nil {
		return false, err
	}
	if im.targetUsers == nil {
		im.targetUsers = map[string]bool{}
	}
	im.targetUsers[name] = ok
	return ok, nil
}

// writeAPISessions copies the sessions with their ids (a session token names its id). Session ids are unique across
// tenants: one another tenant has is skipped (that user logs in again).
func (im *importer) writeAPISessions(w *writer) error {
	count := im.count(entAPISessions)
	d := im.dbs[fileAPISessions]
	if d == nil {
		return nil
	}
	cols := []col{
		c("session_id", kInt), c("expires_at", kTime), c("username", kText), c("last_access_at", kTime),
		c("user_agent", kNText), c("ip_address", kNText),
	}
	taken, orphans := 0, 0
	err := readRows(d, "api_sessions", cols, "session_id", func(vals []any) error {
		count.Read++
		if ok, err := im.targetHasUser(w, vals[2]); err != nil || !ok {
			if err == nil {
				count.Skipped++
				orphans++
			}
			return err
		}
		added, err := w.insertOne("api_sessions", names(cols), vals)
		if err != nil {
			return err
		}
		if added {
			count.Added++
			return nil
		}
		here, err := w.exists("SELECT 1 FROM api_sessions WHERE tenant_id = $1 AND session_id = $2 AND username = $3", vals[0], vals[2])
		if err != nil {
			return err
		}
		if here {
			count.Present++
		} else {
			count.Skipped++
			taken++
		}
		return nil
	})
	if orphans > 0 {
		im.rep.warnf("api sessions: %d sessions of users the tenant doesn't have, not imported", orphans)
	}
	if taken > 0 {
		im.rep.warnf("api sessions: %d session ids are taken by other sessions, not imported (those users log in again)", taken)
	}
	return err
}

// --- library -----------------------------------------------------------------

var (
	entryCols = []col{
		c("id", kText), c("name", kText), c("description", kText), c("tags", kText), c("os", kText),
		c("active_version", kInt), c("builtin_key", kNText), c("created_at", kTime), c("created_by", kText),
		c("updated_at", kTime), c("updated_by", kText), c("deleted_at", kNTime), c("deleted_by", kText),
	}
	versionCols = []col{
		c("entry_id", kText), c("version", kInt), c("created_at", kTime), c("created_by", kText),
		c("message", kText), c("changed", kText), c("content", kText),
	}
)

// writeLibrary copies the library entries with their versions and the library meta. A built-in the target already
// has (the server syncs built-ins into every tenant) counts as present with its versions; an entry whose name a
// different live entry of the target has is skipped.
func (im *importer) writeLibrary(w *writer) error {
	ec, vc := im.count(entLibraryEntries), im.count(entLibraryVersions)
	d := im.dbs[fileLibrary]
	if d == nil {
		im.count(entLibraryMeta)
		return nil
	}
	type builtin struct {
		ID      string `db:"id"`
		Key     string `db:"builtin_key"`
		Deleted bool   `db:"deleted"`
	}
	var have []builtin
	if err := w.tx.SelectContext(w.ctx, &have, "SELECT id, builtin_key, deleted_at IS NOT NULL AS deleted FROM library_entries WHERE tenant_id = $1 AND builtin_key IS NOT NULL", string(w.tid)); err != nil {
		return err
	}
	builtins := map[string]builtin{}
	for _, b := range have {
		builtins[b.Key] = b
	}
	const (
		copied = iota
		asPresent
		skipped
	)
	state := map[string]int{}
	var nameTaken, deletedHere []string
	err := readRows(d, "entries", entryCols, "id", func(vals []any) error {
		ec.Read++
		id := vals[0].(string)
		if key, ok := vals[6].(string); ok {
			if b, ok := builtins[key]; ok && b.ID != id {
				ec.Present++
				state[id] = asPresent
				im.libRemap.add(id, b.ID, key)
				if vals[11] != nil && !b.Deleted {
					deletedHere = append(deletedHere, key)
				}
				return nil
			}
		}
		added, err := w.insertOne("library_entries", names(entryCols), vals)
		if err != nil {
			return err
		}
		if added {
			ec.Added++
			return nil
		}
		here, err := w.exists("SELECT 1 FROM library_entries WHERE tenant_id = $1 AND id = $2", id)
		if err != nil {
			return err
		}
		if here {
			ec.Present++
			return nil
		}
		ec.Skipped++
		state[id] = skipped
		nameTaken = append(nameTaken, vals[1].(string))
		return nil
	})
	if err != nil {
		return err
	}
	if len(nameTaken) > 0 {
		im.rep.warnf("library entries: names a different entry of the target already uses, not imported: %s", strings.Join(nameTaken, ", "))
	}
	if !im.libRemap.empty() {
		im.rep.warnf("library entries: built-ins the target has under other ids, references to them are rewritten to the target's ids: %s", im.libRemap.describe())
	}
	if len(deletedHere) > 0 {
		im.rep.warnf("library entries: built-ins deleted in the source but present in the target (delete them again if they are not wanted): %s", strings.Join(deletedHere, ", "))
	}

	b := w.batch("library_entry_versions", names(versionCols), onConflictNothing, vc)
	err = readRows(d, "entry_versions", versionCols, "entry_id, version", func(vals []any) error {
		vc.Read++
		switch state[vals[0].(string)] {
		case asPresent:
			vc.Present++
			return nil
		case skipped:
			vc.Skipped++
			return nil
		}
		return b.add(vals)
	})
	if err != nil {
		return err
	}
	if err := b.flush(); err != nil {
		return err
	}
	metaCols := []col{c("key", kText), c("value", kText)}
	return im.copyTable(w, entLibraryMeta, fileLibrary, "library_meta", metaCols, names(metaCols), "key")
}

// --- schedules ---------------------------------------------------------------

// writeSchedules copies the schedules, pointing those that run a built-in library entry the target has under another
// id at the target's entry (see libraryRemap); schedules an earlier import left with the source's id are repaired.
func (im *importer) writeSchedules(w *writer) error {
	cols := []col{
		c("id", kText), c("created_at", kTime), c("created_by", kText), c("name", kText), c("schedule", kText),
		c("type", kText), c("details", kText),
	}
	count := im.count(entSchedules)
	d := im.dbs[fileJobs]
	if d == nil {
		return nil
	}
	if ok, err := hasTable(d.db, "schedules"); err != nil || !ok {
		return err
	}
	b := w.batch("schedules", names(cols), onConflictNothing, count)
	rewritten := 0
	var remapped []string
	err := readRows(d, "schedules", cols, "id", func(vals []any) error {
		count.Read++
		n := rewritten
		im.libRemap.rewriteDetails(vals, 6, &rewritten)
		if rewritten > n {
			remapped = append(remapped, vals[0].(string))
		}
		return b.add(vals)
	})
	if err != nil {
		return err
	}
	if err := b.flush(); err != nil {
		return err
	}
	im.reportRewrites(entSchedules, rewritten)
	return im.repairSchedules(w, remapped)
}

// --- history -----------------------------------------------------------------

// historyEntities are the history entities, in report order, with where their rows are.
var historyEntities = []struct{ name, file, table string }{
	{entMultiJobs, fileJobs, "multi_jobs"},
	{entJobs, fileJobs, "jobs"},
	{entNotifications, fileNotifications, "notifications_log"},
	{entAuditLog, fileAuditLog, "auditlog"},
	{entMonitoring, fileMonitoring, "measurements"},
}

func (im *importer) writeHistory(w *writer) error {
	if !im.o.WithHistory {
		for _, h := range historyEntities {
			count := im.count(h.name)
			dbs := []*sourceDB{im.dbs[h.file]}
			if h.file == fileAuditLog {
				dbs = im.audit
			}
			for _, d := range dbs {
				if d == nil {
					continue
				}
				if ok, err := hasTable(d.db, h.table); err != nil || !ok {
					if err != nil {
						return err
					}
					continue
				}
				n, err := countRows(d.db, h.table)
				if err != nil {
					return err
				}
				count.Read += n
			}
			count.Skipped = count.Read
			if count.Read > 0 {
				im.rep.warnf("%s: %d rows not imported (use --with-history)", h.name, count.Read)
			}
		}
		return nil
	}
	for _, step := range []func(*writer) error{im.writeJobs, im.writeNotifications, im.writeAuditLog, im.writeMonitoring} {
		if err := step(w); err != nil {
			return err
		}
	}
	return nil
}

var (
	multiJobCols = []col{c("jid", kText), c("started_at", kTime), c("created_by", kText), c("details", kText), c("schedule_id", kNText)}
	jobCols      = []col{
		c("jid", kText), c("status", kText), c("started_at", kTime), c("finished_at", kNTime), c("created_by", kText),
		c("client_id", kText), c("multi_job_id", kNText), c("details", kText),
	}
)

// writeJobs copies the multi-client jobs (with run_error, the run's error the schedule list reads, taken from
// details) and the jobs. A job of a multi-client job that doesn't exist is skipped: Postgres enforces the reference.
func (im *importer) writeJobs(w *writer) error {
	mc, jc := im.count(entMultiJobs), im.count(entJobs)
	d := im.dbs[fileJobs]
	if d == nil {
		return nil
	}
	b := w.batch("multi_jobs", append(names(multiJobCols), "run_error"), onConflictNothing, mc)
	multiRewritten, jobsRewritten := 0, 0
	err := readRows(d, "multi_jobs", multiJobCols, "jid", func(vals []any) error {
		mc.Read++
		var details struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal([]byte(vals[3].(string)), &details) // not JSON: no error to show
		im.libRemap.rewriteDetails(vals, 3, &multiRewritten)
		return b.add(append(vals, strings.ReplaceAll(details.Error, "\x00", "")))
	})
	if err != nil {
		return err
	}
	im.reportRewrites(entMultiJobs, multiRewritten)
	if err := b.flush(); err != nil {
		return err
	}
	var multi []string
	if err := w.tx.SelectContext(w.ctx, &multi, "SELECT jid FROM multi_jobs WHERE tenant_id = $1", string(w.tid)); err != nil {
		return err
	}
	known := make(map[string]bool, len(multi))
	for _, j := range multi {
		known[j] = true
	}
	jb := w.batch("jobs", names(jobCols), onConflictNothing, jc)
	orphans := 0
	err = readRows(d, "jobs", jobCols, "jid", func(vals []any) error {
		jc.Read++
		if m, ok := vals[6].(string); ok && !known[m] {
			jc.Skipped++
			orphans++
			return nil
		}
		im.libRemap.rewriteDetails(vals, 7, &jobsRewritten)
		return jb.add(vals)
	})
	if err != nil {
		return err
	}
	im.reportRewrites(entJobs, jobsRewritten)
	if orphans > 0 {
		im.rep.warnf("jobs: %d jobs of multi-client jobs that don't exist, not imported", orphans)
	}
	return jb.flush()
}

// targetEmpty reports whether the tenant's table is empty. Rows of a table without a natural key are copied only
// into an empty table (a second run would copy them again); otherwise they are counted as skipped.
func (im *importer) targetEmpty(w *writer, table string, count *EntityCount) (bool, error) {
	has, err := w.exists("SELECT 1 FROM " + table + " WHERE tenant_id = $1")
	if err != nil || !has {
		return !has, err
	}
	count.Skipped = count.Read
	if count.Read > 0 {
		im.rep.warnf("%s: the target tenant already has %s rows; the %d source rows are copied only into an empty table, not imported", count.Name, table, count.Read)
	}
	return false, nil
}

var notificationCols = []col{
	c("rowid", kInt), c("notification_id", kText), c("timestamp", kTime), c("contentType", kText), c("reference_id", kText),
	c("transport", kText), c("recipients", kText), c("state", kText), c("subject", kText), c("body", kText),
	c("out", kText), c("err", kText),
}

// writeNotifications copies the notification log, keeping the order of its entries (SQLite's rowid becomes id). An
// entry has no natural key, so the log is copied only into an empty target.
func (im *importer) writeNotifications(w *writer) error {
	count := im.count(entNotifications)
	d := im.dbs[fileNotifications]
	if d == nil {
		return nil
	}
	n, err := countRows(d.db, "notifications_log")
	if err != nil {
		return err
	}
	count.Read = n
	if ok, err := im.targetEmpty(w, "notifications_log", count); err != nil || !ok {
		return err
	}
	dst := []string{"id", "notification_id", "timestamp", "content_type", "reference_id", "transport", "recipients", "state", "subject", "body", "out", "err"}
	b := w.batch("notifications_log", dst, onConflictNothing, &EntityCount{})
	if err := readRows(d, "notifications_log", notificationCols, "rowid", b.add); err != nil {
		return err
	}
	if err := b.flush(); err != nil {
		return err
	}
	count.Added = b.count.Added
	count.Skipped = count.Read - count.Added
	return nil
}

var auditCols = []col{
	c("timestamp", kTime), c("application", kText), c("action", kText), c("username", kNText), c("remote_ip", kNText),
	c("affected_id", kNText), c("client_id", kNText), c("client_hostname", kNText), c("request", kNText), c("response", kNText),
}

// writeAuditLog copies the current and the rotated audit log files, oldest first, into one table. An entry has no
// natural key, so the log is copied only into an empty target.
func (im *importer) writeAuditLog(w *writer) error {
	count := im.count(entAuditLog)
	for _, d := range im.audit {
		n, err := countRows(d.db, "auditlog")
		if err != nil {
			return err
		}
		count.Read += n
	}
	if len(im.audit) == 0 {
		return nil
	}
	if ok, err := im.targetEmpty(w, "auditlog", count); err != nil || !ok {
		return err
	}
	b := w.batch("auditlog", names(auditCols), "", count)
	for _, d := range im.audit {
		if err := readRows(d, "auditlog", auditCols, "timestamp, rowid", b.add); err != nil {
			return err
		}
	}
	return b.flush()
}

func (im *importer) writeMonitoring(w *writer) error {
	cols := []col{
		c("client_id", kText), c("timestamp", kTime), c("cpu_usage_percent", kFloat), c("memory_usage_percent", kFloat),
		c("io_usage_percent", kFloat), c("processes", kNText), c("mountpoints", kNText), c("net_lan_in", kNInt),
		c("net_lan_out", kNInt), c("net_wan_in", kNInt), c("net_wan_out", kNInt),
	}
	return im.copyTable(w, entMonitoring, fileMonitoring, "measurements", cols, names(cols), "client_id, timestamp")
}
