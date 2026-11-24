// Package dispatch is the client dispatcher: the one way the server does something on a client and
// gets the result back. The API handlers, the job runner and the schedules use a Dispatcher and
// never touch a client's SSH connection.
//
// Local is the implementation for a client connected to this process. Another implementation
// (in another Go module) can be given to the server with ServerOpts.Dispatcher.
package dispatch

import (
	"context"
	"errors"

	"github.com/openrport/openrport/server/clients/clienttunnel"
	"github.com/openrport/openrport/share/comm"
	"github.com/openrport/openrport/share/models"
)

var (
	// ErrClientNotConnected is returned when the client is unknown in the ctx tenant or has no
	// connection.
	ErrClientNotConnected = errors.New("client is not connected")
	// ErrTunnelNotFound is returned when the client has no tunnel with the given ID.
	ErrTunnelNotFound = errors.New("tunnel not found")
)

// Dispatcher does work on a client. A client is identified by the tenant in ctx and its client ID;
// a ctx without a tenant is refused. Errors the client itself reports are *comm.ClientError.
type Dispatcher interface {
	// RunJob sends a command or script job to the client. It returns once the client has started it;
	// the result comes back later on the client connection.
	RunJob(ctx context.Context, clientID string, job *models.Job) (*comm.RunCmdResponse, error)
	// Upload asks the client to fetch file.SourceFilePath and store it at file.DestinationPath. It
	// returns when the client has answered.
	Upload(ctx context.Context, clientID string, file *models.UploadedFile) (*models.UploadResponse, error)
	// IsTunnelAllowed asks the client whether its configuration allows a tunnel to remote (host:port).
	IsTunnelAllowed(ctx context.Context, clientID, remote string) (bool, error)
	// CheckPort asks the client whether req.HostPort accepts connections.
	CheckPort(ctx context.Context, clientID string, req *comm.CheckPortRequest) (*comm.CheckPortResponse, error)
	// StartTunnels opens tunnels to the given remotes and returns them in the same order.
	StartTunnels(ctx context.Context, clientID string, remotes []*models.Remote) ([]*clienttunnel.Tunnel, error)
	// CloseTunnel closes a tunnel. Without force it fails while the tunnel has open connections.
	CloseTunnel(ctx context.Context, clientID, tunnelID string, force bool) error
	// SetTunnelACL replaces the ACL of a tunnel; nil allows every address.
	SetTunnelACL(ctx context.Context, clientID, tunnelID string, acl *string) error
	// RefreshUpdatesStatus asks the client to report its updates status again.
	RefreshUpdatesStatus(ctx context.Context, clientID string) error
	// UpdateAttributes sends new tags and labels to the client.
	UpdateAttributes(ctx context.Context, clientID string, attributes models.Attributes) error
}
