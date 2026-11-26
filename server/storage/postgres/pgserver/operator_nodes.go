package pgserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/server/operator"
)

// The console's per-node view: the reads across organizations of migration 1800, in the system tenant.

type nodeRow struct {
	NodeID       string     `db:"node_id"`
	Region       string     `db:"region"`
	PeerAddress  string     `db:"peer_address"`
	Version      *string    `db:"version"`
	Roles        string     `db:"roles"`
	StartedAt    *time.Time `db:"started_at"`
	HeartbeatAt  time.Time  `db:"heartbeat_at"`
	AgeSeconds   float64    `db:"age_seconds"`
	Online       int64      `db:"n_online"`
	Orgs         int64      `db:"n_orgs"`
	Connects     int64      `db:"n_connects"`
	Disconnects  int64      `db:"n_disconnects"`
	TodayIn      int64      `db:"today_in"`
	TodayOut     int64      `db:"today_out"`
	PeriodIn     int64      `db:"period_in"`
	PeriodOut    int64      `db:"period_out"`
	MeteredSince *time.Time `db:"metered_since"`
}

func (r nodeRow) node() operator.Node {
	n := operator.Node{NodeID: r.NodeID, Region: r.Region, PeerAddress: r.PeerAddress,
		StartedAt: utc(r.StartedAt), HeartbeatAt: r.HeartbeatAt.UTC(),
		Status:        operator.NodeStatus(time.Duration(r.AgeSeconds * float64(time.Second))),
		OnlineDevices: r.Online, Organizations: r.Orgs, Connects24h: r.Connects, Disconnects24h: r.Disconnects,
		TransferToday:  operator.Transfer{In: r.TodayIn, Out: r.TodayOut},
		TransferPeriod: operator.Transfer{In: r.PeriodIn, Out: r.PeriodOut}}
	if err := json.Unmarshal([]byte(r.Roles), &n.Roles); err != nil || n.Roles == nil {
		n.Roles = []string{}
	}
	if r.Version != nil {
		n.Version = *r.Version
	}
	if r.MeteredSince != nil {
		s := r.MeteredSince.Format(time.DateOnly)
		n.MeteredSince = &s
	}
	return n
}

// nodeColumns are the columns of rport_operator_nodes as nodeRow scans them (roles as a JSON array).
const nodeColumns = `node_id, region, peer_address, version, coalesce(array_to_json(roles)::text, '[]') AS roles, started_at,
	heartbeat_at, age_seconds, n_online, n_orgs, n_connects, n_disconnects, today_in, today_out, period_in, period_out,
	metered_since`

func dateArg(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// Nodes implements operator.Service: two statements, whatever the number of nodes.
func (o *Operator) Nodes(ctx context.Context, from, to time.Time) ([]operator.Node, int64, error) {
	var out []operator.Node
	var unassigned int64
	err := o.inSystem(ctx, func(tx *sqlx.Tx) error {
		var rows []nodeRow
		if err := tx.SelectContext(ctx, &rows, "SELECT "+nodeColumns+" FROM rport_operator_nodes($1::date, $2::date)", dateArg(from), dateArg(to)); err != nil {
			return err
		}
		out = make([]operator.Node, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.node())
		}
		return tx.GetContext(ctx, &unassigned, "SELECT rport_operator_unassigned_sessions()")
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list nodes: %w", err)
	}
	return out, unassigned, nil
}

// Node implements operator.Service.
func (o *Operator) Node(ctx context.Context, id string, from, to time.Time) (*operator.NodeDetail, error) {
	var out operator.NodeDetail
	err := o.inSystem(ctx, func(tx *sqlx.Tx) error {
		var rows []nodeRow
		if err := tx.SelectContext(ctx, &rows, "SELECT "+nodeColumns+" FROM rport_operator_nodes($1::date, $2::date) WHERE node_id = $3",
			dateArg(from), dateArg(to), id); err != nil {
			return err
		}
		if len(rows) == 0 {
			return operator.ErrNodeNotFound
		}
		out.Node = rows[0].node()

		var days []struct {
			Day         time.Time `db:"day"`
			In          int64     `db:"bytes_in"`
			Out         int64     `db:"bytes_out"`
			Connects    int64     `db:"n_connects"`
			Disconnects int64     `db:"n_disconnects"`
		}
		if err := tx.SelectContext(ctx, &days, "SELECT * FROM rport_operator_node_days($1, $2::date, $3::date)",
			id, dateArg(from), dateArg(to)); err != nil {
			return err
		}
		out.Daily = make([]operator.NodeDay, 0, len(days))
		for _, d := range days {
			out.Daily = append(out.Daily, operator.NodeDay{Day: d.Day.Format(time.DateOnly), In: d.In, Out: d.Out,
				Connects: d.Connects, Disconnects: d.Disconnects})
		}

		var orgs []struct {
			Org    string `db:"org"`
			Name   string `db:"org_name"`
			Online int64  `db:"n_online"`
		}
		if err := tx.SelectContext(ctx, &orgs, "SELECT * FROM rport_operator_node_organizations($1)", id); err != nil {
			return err
		}
		out.Organizations = make([]operator.NodeOrganization, 0, len(orgs))
		for _, x := range orgs {
			out.Organizations = append(out.Organizations, operator.NodeOrganization{Organization: x.Org, OrganizationName: x.Name, OnlineDevices: x.Online})
		}

		var events []struct {
			At     time.Time `db:"at"`
			Org    string    `db:"org"`
			Client string    `db:"client_id"`
			Name   string    `db:"device_name"`
			Event  string    `db:"event"`
		}
		if err := tx.SelectContext(ctx, &events, "SELECT * FROM rport_operator_node_events($1, $2)", id, operator.NodeEventsLimit); err != nil {
			return err
		}
		out.Events = make([]operator.NodeEvent, 0, len(events))
		for _, e := range events {
			out.Events = append(out.Events, operator.NodeEvent{At: e.At.UTC(), Organization: e.Org, ClientID: e.Client, DeviceName: e.Name, Event: e.Event})
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, operator.ErrNodeNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("node %q: %w", id, err)
	}
	return &out, nil
}

// NodeDevices implements operator.Service. Only the listed keys of the stored documents leave the query.
func (o *Operator) NodeDevices(ctx context.Context, id string, limit, offset int) ([]operator.NodeDevice, int, error) {
	var out []operator.NodeDevice
	total := 0
	err := o.inSystem(ctx, func(tx *sqlx.Tx) error {
		var known bool
		if err := tx.GetContext(ctx, &known, "SELECT EXISTS (SELECT 1 FROM nodes WHERE node_id = $1)", id); err != nil {
			return err
		}
		if !known {
			return operator.ErrNodeNotFound
		}
		var counts []int
		if err := tx.SelectContext(ctx, &counts, "SELECT n_online FROM rport_operator_node_organizations($1)", id); err != nil {
			return err
		}
		for _, n := range counts {
			total += n
		}
		var rows []struct {
			Org         string     `db:"org"`
			OrgName     string     `db:"org_name"`
			ID          string     `db:"id"`
			Name        string     `db:"name"`
			Version     string     `db:"version"`
			OSFullName  string     `db:"os_full_name"`
			OS          string     `db:"os"`
			OSFamily    string     `db:"os_family"`
			ConnectedAt time.Time  `db:"connected_at"`
			FirstSeenAt *time.Time `db:"first_seen_at"`
			Now         time.Time  `db:"now"`
		}
		if err := tx.SelectContext(ctx, &rows, "SELECT d.*, now() AS now FROM rport_operator_node_devices($1, $2, $3) d",
			id, limit, offset); err != nil {
			return err
		}
		out = make([]operator.NodeDevice, 0, len(rows))
		for _, r := range rows {
			node := id
			d := operator.Device{ID: r.ID, Name: r.Name, Online: true, NodeID: &node, Version: r.Version, OS: r.OSFullName,
				OSFamily: r.OSFamily, ConnectedAt: utc(&r.ConnectedAt), LastSeenAt: utc(&r.Now), FirstSeenAt: utc(r.FirstSeenAt)}
			if d.OS == "" {
				d.OS = r.OS
			}
			out = append(out, operator.NodeDevice{Organization: r.Org, OrganizationName: r.OrgName, Device: d})
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, operator.ErrNodeNotFound) {
			return nil, 0, err
		}
		return nil, 0, fmt.Errorf("devices of node %q: %w", id, err)
	}
	return out, total, nil
}
