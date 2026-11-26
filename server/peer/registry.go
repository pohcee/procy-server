// Package peer passes work on clients between the processes of a cluster. An api process finds the device process
// holding a client's connection in the session registry and calls it over the peer API (HTTP/2 over mutual TLS,
// JSON bodies, newline-delimited JSON for streamed answers); the device process runs the work with its local
// dispatcher. Certificates carry the role of the process (see Role), and a device process accepts work only from api
// certificates.
//
// The pieces: Tracker (device side) records the sessions of the clients connected to the process in the registry
// and closes those another node took over; Server (device side) answers peer calls; Dispatcher (api side) is the
// dispatch.Dispatcher that makes them.
package peer

import (
	"context"
	"time"

	"github.com/pohcee/procy-server/share/tenant"
)

// Session is one client connection held by a device process.
type Session struct {
	Tenant   tenant.ID
	ClientID string
	// SessionID is unique per connection.
	SessionID string
}

// Location is the device process holding a client's connection.
type Location struct {
	NodeID      string
	PeerAddress string
}

// NodeInfo is what a device process reports about itself.
type NodeInfo struct {
	NodeID      string
	Region      string
	PeerAddress string
	// Version is the build version of the process, Roles its [storage] role ("all" or "device"), StartedAt when
	// it started: shown to operators.
	Version   string
	Roles     []string
	StartedAt time.Time
}

// SessionStore is the registry as a device process writes it.
type SessionStore interface {
	// Heartbeat records that the node is up and where other processes reach it.
	Heartbeat(ctx context.Context, node NodeInfo) error
	// Register records a new connection; it replaces any other session of the client (the newest wins).
	Register(ctx context.Context, nodeID string, s Session, lease time.Duration) error
	// Renew extends the leases of the sessions held by the node and returns the ids of those another session
	// replaced.
	Renew(ctx context.Context, nodeID string, held []Session, lease time.Duration) (lost []string, err error)
	// End removes a session whose connection ended. A newer session of the client stays.
	End(ctx context.Context, sessionID string) error
}

// Locator finds where a client is connected. The tenant comes from ctx. It returns nil, nil when the client has no
// live session anywhere.
type Locator interface {
	Locate(ctx context.Context, clientID string) (*Location, error)
}
