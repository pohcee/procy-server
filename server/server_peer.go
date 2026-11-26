package chserver

import (
	"context"

	"github.com/pohcee/procy-server/server/clients"
	"github.com/pohcee/procy-server/server/dispatch"
	"github.com/pohcee/procy-server/server/jobrunner"
	"github.com/pohcee/procy-server/share/ws"
)

// PeerNode runs what a process of a cluster needs besides the listeners once the server is built: the peer listener
// other processes pass work on clients to, the session registry upkeep and the dispatcher's view of the server
// (ServerOpts.Peer). nil in standalone.
type PeerNode interface {
	// Bind is called by NewServer with the parts of the built server the node works with.
	Bind(PeerBinding) error
	// Start starts the node's listeners and loops; they run until ctx ends or Close.
	Start(ctx context.Context) error
	Close() error
}

// PeerBinding is what a PeerNode gets from the server.
type PeerBinding struct {
	// Local does work on the clients connected to this process (whatever ServerOpts.Dispatcher is).
	Local dispatch.Dispatcher
	// HasClientListener is false for a server without client listener (ServerOpts.DisableClientListener).
	HasClientListener bool
	Clients           clients.ClientService
	// UIJobStreams are the websockets job output is streamed to; JobsDone the sequential runs waiting for results.
	UIJobStreams *ws.WebSocketCache
	JobsDone     *jobrunner.DoneChannels
	// Jobs is the job storage.
	Jobs JobProvider
}

func (s *Server) bindPeer(p PeerNode) error {
	if p == nil {
		return nil
	}
	s.peer = p
	return p.Bind(PeerBinding{
		Local:             s.localDispatcher,
		HasClientListener: s.clientListener != nil,
		Clients:           s.clientService,
		UIJobStreams:      &s.uiJobWebSockets,
		JobsDone:          s.jobsDoneChannel,
		Jobs:              s.jobProvider,
	})
}
