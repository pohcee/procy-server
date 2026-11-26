package operator

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gorilla/mux"

	"github.com/pohcee/procy-server/server/api"
	"github.com/pohcee/procy-server/server/peer"
	"github.com/pohcee/procy-server/share/query"
)

// The console's per-node view (Servers): the nodes of the cluster registry with what they serve. Counts and the
// devices' metadata only, as Device.

// ErrNodeNotFound: no node with that id in the cluster registry.
var ErrNodeNotFound = errors.New("node not found")

// Node statuses, from the age of the node's last heartbeat.
const (
	NodeLive  = "live"
	NodeStale = "stale"
	NodeGone  = "gone"
)

const (
	// nodeLiveFor is how old a heartbeat may be for its node to count as live: three heartbeat intervals.
	nodeLiveFor = 3 * peer.DefaultRenewInterval
	// nodeStaleFor is how old it may be for a stale node; older is gone.
	nodeStaleFor = 24 * time.Hour
	// defaultNodeDays and maxNodeDays are the default and the longest period of the node views.
	defaultNodeDays = 30
	maxNodeDays     = 90
	// NodeEventsLimit is how many connection events the node detail lists.
	NodeEventsLimit = 50
	// defaultNodeDevices and maxNodeDevices are the default and the largest page of a node's devices.
	defaultNodeDevices = 50
	maxNodeDevices     = 500
)

// NodeStatus is the status of a node whose last heartbeat is age old.
func NodeStatus(age time.Duration) string {
	switch {
	case age <= nodeLiveFor:
		return NodeLive
	case age <= nodeStaleFor:
		return NodeStale
	}
	return NodeGone
}

var nodeStatusOrder = map[string]int{NodeLive: 0, NodeStale: 1, NodeGone: 2}

// Node is a node of the cluster registry (a process with a client listener) with what it serves.
type Node struct {
	NodeID string `json:"node_id"`
	Region string `json:"region"`
	// PeerAddress is where other processes reach it ("" when it answers no peer calls).
	PeerAddress string `json:"peer_address"`
	// Roles and Version are what its process reported (empty: a release that doesn't report them).
	Roles   []string `json:"roles"`
	Version string   `json:"version"`
	// StartedAt is when its process started (nil: not reported).
	StartedAt   *time.Time `json:"started_at"`
	HeartbeatAt time.Time  `json:"heartbeat_at"`
	Status      string     `json:"status"`
	// OnlineDevices are its live sessions, Organizations the organizations they belong to.
	OnlineDevices  int64 `json:"online_devices"`
	Organizations  int64 `json:"organizations"`
	Connects24h    int64 `json:"connects_24h"`
	Disconnects24h int64 `json:"disconnects_24h"`
	// TransferToday is its transfer on the current UTC day, TransferPeriod over the period.
	TransferToday  Transfer `json:"transfer_today"`
	TransferPeriod Transfer `json:"transfer_period"`
	// MeteredSince is the first UTC day with transfer counted for it (YYYY-MM-DD; nil: none).
	MeteredSince *string `json:"metered_since"`
}

// NodeTotals are the totals of the node list.
type NodeTotals struct {
	// OnlineDevices are every live session: the listed nodes' and the unassigned ones.
	OnlineDevices int64 `json:"online_devices"`
	// TransferPeriod is the listed nodes' transfer over the period.
	TransferPeriod Transfer `json:"transfer_period"`
}

// NodeList is GET /operator/nodes.
type NodeList struct {
	Nodes  []Node     `json:"nodes"`
	Totals NodeTotals `json:"totals"`
	// UnassignedOnline are the live sessions whose node is not in the registry.
	UnassignedOnline int64 `json:"unassigned_online"`
	Days             int   `json:"days"`
}

// NodeDay is a UTC day of a node's series.
type NodeDay struct {
	Day         string `json:"day"`
	In          int64  `json:"in"`
	Out         int64  `json:"out"`
	Connects    int64  `json:"connects"`
	Disconnects int64  `json:"disconnects"`
}

// NodeOrganization is an organization with live sessions on a node.
type NodeOrganization struct {
	Organization     string `json:"organization"`
	OrganizationName string `json:"organization_name"`
	OnlineDevices    int64  `json:"online_devices"`
}

// NodeEvent is a connection event of a node.
type NodeEvent struct {
	At           time.Time `json:"at"`
	Organization string    `json:"organization"`
	ClientID     string    `json:"client_id"`
	DeviceName   string    `json:"device_name"`
	// Event is "connected" or "disconnected".
	Event string `json:"event"`
}

// NodeDetail is GET /operator/nodes/{node_id}.
type NodeDetail struct {
	Node          Node               `json:"node"`
	Daily         []NodeDay          `json:"daily"`
	Organizations []NodeOrganization `json:"organizations"`
	Events        []NodeEvent        `json:"events"`
}

// NodeDevice is a device with a live session on a node: Device with its organization.
type NodeDevice struct {
	Organization     string `json:"organization"`
	OrganizationName string `json:"organization_name"`
	Device
}

// mountNodes registers the per-node routes.
func (h *Handler) mountNodes(r *mux.Router) {
	r.HandleFunc("/nodes", h.nodes).Methods(http.MethodGet)
	r.HandleFunc("/nodes/{node_id}", h.node).Methods(http.MethodGet)
	r.HandleFunc("/nodes/{node_id}/devices", h.nodeDevices).Methods(http.MethodGet)
}

// nodePeriod parses ?days= (1 to 90, default 30) and returns the period's UTC days [from, to): to is tomorrow.
func nodePeriod(r *http.Request) (days int, from, to time.Time, err error) {
	days = defaultNodeDays
	if s := r.URL.Query().Get("days"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxNodeDays {
			return 0, time.Time{}, time.Time{}, fmt.Errorf("%w: days must be 1 to %d", ErrInvalid, maxNodeDays)
		}
		days = n
	}
	to = startOfDay(now()).AddDate(0, 0, 1)
	return days, to.AddDate(0, 0, -days), to, nil
}

func sortNodes(ns []Node) {
	sort.SliceStable(ns, func(i, j int) bool {
		if a, b := nodeStatusOrder[ns[i].Status], nodeStatusOrder[ns[j].Status]; a != b {
			return a < b
		}
		return ns[i].NodeID < ns[j].NodeID
	})
}

func (h *Handler) nodes(w http.ResponseWriter, r *http.Request) {
	days, from, to, err := nodePeriod(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	ns, unassigned, err := h.svc.Nodes(r.Context(), from, to)
	if err != nil {
		h.fail(w, err)
		return
	}
	if ns == nil {
		ns = []Node{}
	}
	sortNodes(ns)
	out := NodeList{Nodes: ns, UnassignedOnline: unassigned, Days: days, Totals: NodeTotals{OnlineDevices: unassigned}}
	for i, n := range ns {
		if n.Roles == nil {
			ns[i].Roles = []string{}
		}
		out.Totals.OnlineDevices += n.OnlineDevices
		out.Totals.TransferPeriod.In += n.TransferPeriod.In
		out.Totals.TransferPeriod.Out += n.TransferPeriod.Out
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(out))
}

func (h *Handler) node(w http.ResponseWriter, r *http.Request) {
	_, from, to, err := nodePeriod(r)
	if err != nil {
		h.fail(w, err)
		return
	}
	d, err := h.svc.Node(r.Context(), mux.Vars(r)["node_id"], from, to)
	if err != nil {
		h.fail(w, err)
		return
	}
	have := make(map[string]NodeDay, len(d.Daily))
	for _, x := range d.Daily {
		have[x.Day] = x
	}
	daily := make([]NodeDay, 0, int(to.Sub(from)/day))
	for t := from; t.Before(to); t = t.AddDate(0, 0, 1) {
		k := t.Format(time.DateOnly)
		x := have[k]
		x.Day = k
		daily = append(daily, x)
	}
	d.Daily = daily
	if d.Node.Roles == nil {
		d.Node.Roles = []string{}
	}
	if d.Organizations == nil {
		d.Organizations = []NodeOrganization{}
	}
	if d.Events == nil {
		d.Events = []NodeEvent{}
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(d))
}

func (h *Handler) nodeDevices(w http.ResponseWriter, r *http.Request) {
	p := query.ParsePagination(r.URL.Query())
	if errs := query.ValidatePagination(p, &query.PaginationConfig{MaxLimit: maxNodeDevices, DefaultLimit: defaultNodeDevices}); errs != nil {
		h.fail(w, errs)
		return
	}
	ds, total, err := h.svc.NodeDevices(r.Context(), mux.Vars(r)["node_id"], p.ValidatedLimit, p.ValidatedOffset)
	if err != nil {
		h.fail(w, err)
		return
	}
	if ds == nil {
		ds = []NodeDevice{}
	}
	h.respond.JSON(w, http.StatusOK, &api.SuccessPayload{Data: ds, Meta: api.NewMeta(total)})
}
