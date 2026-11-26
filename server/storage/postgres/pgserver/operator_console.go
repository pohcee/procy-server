package pgserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/server/operator"
	"github.com/pohcee/procy-server/server/storage/postgres"
)

// The reads of the operator console. Reads across organizations go through the SECURITY DEFINER functions of
// migration 1750 (one statement for every organization); reads of one organization switch the transaction to its
// tenant, so row-level security applies as for its own users.

// Organizations implements operator.Service: two statements, whatever the number of organizations.
func (o *Operator) Organizations(ctx context.Context) ([]operator.OrganizationDetail, error) {
	var out []operator.OrganizationDetail
	err := o.inSystem(ctx, func(tx *sqlx.Tx) error {
		var orgs []operator.Organization
		if err := tx.SelectContext(ctx, &orgs,
			"SELECT "+organizationColumns+" FROM tenants WHERE left(id, 1) <> '_' ORDER BY created_at, id"); err != nil {
			return err
		}
		var rows []struct {
			Org         string     `db:"org"`
			Devices     int        `db:"n_devices"`
			Online      int        `db:"n_online"`
			Users       int        `db:"n_users"`
			LastDevice  *time.Time `db:"last_device"`
			LastLogin   *time.Time `db:"last_login"`
			Invitations int        `db:"n_invitations"`
			FromSignup  bool       `db:"from_signup"`
		}
		if err := tx.SelectContext(ctx, &rows, "SELECT * FROM rport_operator_organization_counts($1)", o.cutoff()); err != nil {
			return err
		}
		byID := make(map[string]int, len(rows))
		for i, r := range rows {
			byID[r.Org] = i
		}
		out = make([]operator.OrganizationDetail, 0, len(orgs))
		for _, org := range orgs {
			d := operator.OrganizationDetail{Organization: org}
			if i, ok := byID[org.ID]; ok {
				r := rows[i]
				d.DeviceCount, d.UserCount = r.Devices, r.Users
				d.Activity = operator.Activity{DevicesOnline: r.Online, LastDeviceAt: utc(r.LastDevice), LastLoginAt: utc(r.LastLogin),
					InvitationsPending: r.Invitations, FromSignup: r.FromSignup}
			}
			out = append(out, d)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list organizations: %w", err)
	}
	return out, nil
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// UsageDays implements operator.Service.
func (o *Operator) UsageDays(ctx context.Context, from, to time.Time) ([]operator.DayUsage, error) {
	var out []operator.DayUsage
	err := o.inSystem(ctx, func(tx *sqlx.Tx) error {
		var rows []struct {
			Day     time.Time `db:"day"`
			In      int64     `db:"bytes_in"`
			Out     int64     `db:"bytes_out"`
			Devices int64     `db:"devices"`
		}
		if err := tx.SelectContext(ctx, &rows, "SELECT * FROM rport_operator_usage_days($1::date, $2::date)",
			from.UTC().Format(time.DateOnly), to.UTC().Format(time.DateOnly)); err != nil {
			return err
		}
		for _, r := range rows {
			d := r.Day
			out = append(out, operator.DayUsage{Day: time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC), In: r.In, Out: r.Out, Devices: r.Devices})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("platform usage per day: %w", err)
	}
	return out, nil
}

// AwaitingSignups implements operator.Service: signups live in the system tenant.
func (o *Operator) AwaitingSignups(ctx context.Context) ([]time.Time, error) {
	var out []time.Time
	err := o.inSystem(ctx, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &out, `SELECT coalesce(verified_at, created_at) FROM signups
			WHERE tenant_id = $1 AND status = 'awaiting_approval' ORDER BY 1`, string(operator.SystemTenant))
	})
	if err != nil {
		return nil, fmt.Errorf("signups awaiting approval: %w", err)
	}
	for i := range out {
		out[i] = out[i].UTC()
	}
	return out, nil
}

// organizationExists checks that id is an organization (not a reserved tenant), without leaving the system tenant.
func organizationExists(ctx context.Context, tx *sqlx.Tx, id string) error {
	if strings.HasPrefix(id, "_") {
		return operator.ErrNotFound
	}
	var name string
	err := tx.GetContext(ctx, &name, "SELECT name FROM tenants WHERE id = $1", id)
	if errors.Is(err, sql.ErrNoRows) {
		return operator.ErrNotFound
	}
	return err
}

// Devices implements operator.Service. Only the listed keys of the stored documents leave the query.
func (o *Operator) Devices(ctx context.Context, id string) ([]operator.Device, error) {
	var out []operator.Device
	err := o.inSystem(ctx, func(tx *sqlx.Tx) error {
		if err := organizationExists(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "SELECT set_config($1, $2, true)", postgres.TenantSetting, id); err != nil {
			return err
		}
		var rows []struct {
			ID             string     `db:"id"`
			Name           string     `db:"name"`
			Version        string     `db:"version"`
			OSFullName     string     `db:"os_full_name"`
			OS             string     `db:"os"`
			OSFamily       string     `db:"os_family"`
			HasRecord      bool       `db:"has_record"`
			DisconnectedAt *time.Time `db:"disconnected_at"`
			NodeID         *string    `db:"node_id"`
			ConnectedAt    *time.Time `db:"connected_at"`
			FirstSeenAt    *time.Time `db:"first_seen_at"`
			Now            time.Time  `db:"now"`
		}
		if err := tx.SelectContext(ctx, &rows, `SELECT c.id,
				coalesce(r.doc ->> 'name', '') AS name, coalesce(r.doc ->> 'version', '') AS version,
				coalesce(r.doc ->> 'os_full_name', '') AS os_full_name, coalesce(r.doc ->> 'os', '') AS os,
				coalesce(r.doc ->> 'os_family', '') AS os_family,
				r.id IS NOT NULL AS has_record, r.disconnected_at, s.node_id, s.connected_at, st.first_seen_at, now() AS now
			FROM rport_counted_clients($1, $2) AS c(id)
			LEFT JOIN LATERAL (SELECT x.id, x.disconnected_at, x.details::jsonb AS doc FROM client_records x
			                   WHERE x.tenant_id = $1 AND x.id = c.id) r ON true
			LEFT JOIN client_sessions s ON s.tenant_id = $1 AND s.client_id = c.id AND s.lease_until > now()
			LEFT JOIN client_state st ON st.tenant_id = $1 AND st.client_id = c.id
			ORDER BY c.id`, id, o.cutoff()); err != nil {
			return err
		}
		out = make([]operator.Device, 0, len(rows))
		for _, r := range rows {
			d := operator.Device{ID: r.ID, Name: r.Name, Version: r.Version, OS: r.OSFullName, OSFamily: r.OSFamily,
				NodeID: r.NodeID, ConnectedAt: utc(r.ConnectedAt), FirstSeenAt: utc(r.FirstSeenAt)}
			if d.OS == "" {
				d.OS = r.OS
			}
			d.Online = r.NodeID != nil || (r.HasRecord && r.DisconnectedAt == nil)
			if d.Online {
				d.LastSeenAt = utc(&r.Now)
			} else {
				d.LastSeenAt = utc(r.DisconnectedAt)
			}
			out = append(out, d)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, operator.ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("devices of %q: %w", id, err)
	}
	return out, nil
}

type invitationRow struct {
	Org       string    `db:"org"`
	OrgName   string    `db:"org_name"`
	Login     string    `db:"login"`
	Groups    string    `db:"groups_json"`
	InvitedBy string    `db:"invited_by"`
	InvitedAt time.Time `db:"invited_at"`
}

func (r invitationRow) invitation() operator.Invitation {
	inv := operator.Invitation{Organization: r.Org, OrganizationName: r.OrgName, Username: r.Login, Groups: []string{},
		InvitedBy: r.InvitedBy, CreatedAt: r.InvitedAt.UTC()}
	if err := json.Unmarshal([]byte(r.Groups), &inv.Groups); err != nil || inv.Groups == nil {
		inv.Groups = []string{}
	}
	return inv
}

// Invitations implements operator.Service.
func (o *Operator) Invitations(ctx context.Context, id string) ([]operator.Invitation, error) {
	var out []operator.Invitation
	err := o.inSystem(ctx, func(tx *sqlx.Tx) error {
		var only *string
		if id != "" {
			if err := organizationExists(ctx, tx, id); err != nil {
				return err
			}
			only = &id
		}
		var rows []invitationRow
		if err := tx.SelectContext(ctx, &rows, "SELECT * FROM rport_operator_invitations($1)", only); err != nil {
			return err
		}
		out = make([]operator.Invitation, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.invitation())
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, operator.ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("invitations: %w", err)
	}
	return out, nil
}

// ResendInvitation implements operator.Service.
func (o *Operator) ResendInvitation(ctx context.Context, by operator.Actor, id, username string) (*operator.Invitation, error) {
	var inv operator.Invitation
	err := o.invitationChange(ctx, by, id, username, operator.AuditActionResend, func(tx *sqlx.Tx) error {
		var r invitationRow
		err := tx.GetContext(ctx, &r, `UPDATE membership_invitations v SET created_at = now(), invited_by = $3
			FROM tenants t WHERE t.id = v.tenant_id AND v.tenant_id = $1 AND v.username = $2
			RETURNING v.tenant_id AS org, t.name AS org_name, v.username AS login, v.groups AS groups_json, v.invited_by, v.created_at AS invited_at`,
			id, username, "operator:"+by.Username)
		if errors.Is(err, sql.ErrNoRows) {
			return operator.ErrNoInvitation
		}
		inv = r.invitation()
		return err
	})
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

// RevokeInvitation implements operator.Service.
func (o *Operator) RevokeInvitation(ctx context.Context, by operator.Actor, id, username string) error {
	return o.invitationChange(ctx, by, id, username, operator.AuditActionDelete, func(tx *sqlx.Tx) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM membership_invitations WHERE tenant_id = $1 AND username = $2", id, username)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			if err != nil {
				return err
			}
			return operator.ErrNoInvitation
		}
		return nil
	})
}

// invitationChange runs fn in the organization's tenant, then writes the operator's action to the system tenant's
// audit log, in one transaction.
func (o *Operator) invitationChange(ctx context.Context, by operator.Actor, id, username, action string, fn func(tx *sqlx.Tx) error) error {
	err := o.inSystem(ctx, func(tx *sqlx.Tx) error {
		if err := organizationExists(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "SELECT set_config($1, $2, true)", postgres.TenantSetting, id); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		return o.audit(ctx, tx, by, operator.AuditApplicationOrganizationInvitation, action, id, map[string]string{"username": username})
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, operator.ErrNotFound), errors.Is(err, operator.ErrNoInvitation):
		return err
	default:
		return fmt.Errorf("invitation of %q to %q: %w", username, id, err)
	}
}

// activityFilter is the SQL condition on auditlog of a kind of activity.
func activityFilter(kind operator.ActivityKind) (string, error) {
	quote := func(vs []string) string {
		q := make([]string, len(vs))
		for i, v := range vs {
			q[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
		}
		return strings.Join(q, ", ")
	}
	switch kind {
	case "", operator.ActivityAll:
		return "true", nil
	case operator.ActivitySignIn:
		return "application IN (" + quote(operator.SignInApplications) + ")", nil
	case operator.ActivityAdmin:
		cond := "application NOT IN (" + quote(append(append([]string(nil), operator.SignInApplications...), operator.DeviceWorkApplications...)) + ")"
		for _, p := range operator.DeviceWorkPrefixes {
			cond += " AND left(application, " + fmt.Sprint(len(p)) + ") <> " + quote([]string{p})
		}
		return cond, nil
	}
	return "", fmt.Errorf("%w: kind must be all, sign_in or admin", operator.ErrInvalid)
}

// operatorActions are the system tenant's audit entries about organizations: the operator API's, billing's and
// signup's. Other system entries (operators' sign-ins, their user changes) name logins in affected_id, which may
// equal an organization id.
const operatorActions = `(application LIKE 'operator.%' OR application IN ('billing', 'signup', 'organization.deletion', 'organization.export'))`

// Activity implements operator.Service: the organization's audit log (in its tenant) and the system tenant's
// entries about it, each read up to the limit, merged newest first. request and response are never read.
func (o *Operator) Activity(ctx context.Context, id string, q operator.ActivityQuery) ([]operator.ActivityEntry, error) {
	filter, err := activityFilter(q.Kind)
	if err != nil {
		return nil, err
	}
	if q.Limit <= 0 {
		return []operator.ActivityEntry{}, nil
	}
	var before *time.Time
	var beforeID int64
	if q.Before != nil {
		before, beforeID = &q.Before.Timestamp, q.Before.ID
	}
	const columns = `id, timestamp, application, action, coalesce(username, '') AS username,
		coalesce(remote_ip, '') AS remote_ip, coalesce(affected_id, '') AS affected_id`
	page := "($2::timestamptz IS NULL OR (timestamp, id) < ($2::timestamptz, $3::bigint)) AND " + filter +
		" ORDER BY timestamp DESC, id DESC LIMIT $4"
	var entries []operator.ActivityEntry
	err = o.inSystem(ctx, func(tx *sqlx.Tx) error {
		if err := organizationExists(ctx, tx, id); err != nil {
			return err
		}
		var system []operator.ActivityEntry
		if err := tx.SelectContext(ctx, &system, "SELECT "+columns+" FROM auditlog WHERE tenant_id = $5 AND affected_id = $1 AND "+operatorActions+" AND "+page,
			id, before, beforeID, q.Limit, string(operator.SystemTenant)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "SELECT set_config($1, $2, true)", postgres.TenantSetting, id); err != nil {
			return err
		}
		var own []operator.ActivityEntry
		if err := tx.SelectContext(ctx, &own, "SELECT "+columns+" FROM auditlog WHERE tenant_id = $1 AND "+page,
			id, before, beforeID, q.Limit); err != nil {
			return err
		}
		entries = mergeActivity(own, system, q.Limit)
		return nil
	})
	if err != nil {
		if errors.Is(err, operator.ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("activity of %q: %w", id, err)
	}
	return entries, nil
}

// mergeActivity merges two pages, each newest first, into one of at most limit entries.
func mergeActivity(own, system []operator.ActivityEntry, limit int) []operator.ActivityEntry {
	for i := range own {
		own[i].Source, own[i].Timestamp = operator.SourceOrganization, own[i].Timestamp.UTC()
	}
	for i := range system {
		system[i].Source, system[i].Timestamp = operator.SourceOperator, system[i].Timestamp.UTC()
	}
	newer := func(a, b operator.ActivityEntry) bool {
		if !a.Timestamp.Equal(b.Timestamp) {
			return a.Timestamp.After(b.Timestamp)
		}
		return a.ID > b.ID
	}
	out := make([]operator.ActivityEntry, 0, min(limit, len(own)+len(system)))
	i, j := 0, 0
	for len(out) < limit && (i < len(own) || j < len(system)) {
		if j >= len(system) || (i < len(own) && newer(own[i], system[j])) {
			out = append(out, own[i])
			i++
		} else {
			out = append(out, system[j])
			j++
		}
	}
	return out
}
