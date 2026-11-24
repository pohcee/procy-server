package pgserver

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	chserver "github.com/openrport/openrport/server"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/peer"
	pgclients "github.com/openrport/openrport/server/storage/postgres/clients"
	"github.com/openrport/openrport/server/storage/postgres/clientsessions"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

// sweepInterval is how often clients stored as connected without a live session are marked disconnected (one node
// per tenant at a time).
const sweepInterval = 30 * time.Second

// clusterProcess is the peer side of a Postgres node (chserver.PeerNode): the session tracker of a process with a
// client listener, the peer listener of a process that answers peer calls, the cluster dispatcher of a process that
// makes them, and the sweep of sessions whose node went away.
type clusterProcess struct {
	cfg        *chconfig.Config
	log        *logger.Logger
	coord      *Coordinator
	clients    *pgclients.Store
	tracker    *peer.Tracker
	dispatcher *peer.Dispatcher
	auth       *peer.Auth

	server *peer.Server
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var _ chserver.PeerNode = (*clusterProcess)(nil)

// peerAuth returns the peer authentication of [cluster], checking that this process's certificate is for its node
// and carries the roles its storage role needs.
func peerAuth(cfg *chconfig.Config) (*peer.Auth, error) {
	cl := cfg.Cluster
	if cl.PeerSecretFile != "" {
		return peer.NewInsecureSecretAuth(cl.PeerSecret), nil
	}
	auth, err := peer.NewTLSAuth(cl.TrustDomain, cl.CAFile, cl.CertFile, cl.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("[cluster]: %w", err)
	}
	id, err := auth.Identity()
	if err != nil {
		return nil, fmt.Errorf("[cluster] cert_file: %w", err)
	}
	if id.NodeID != cl.NodeID {
		return nil, fmt.Errorf("[cluster] cert_file is for node %q, but node_id is %q", id.NodeID, cl.NodeID)
	}
	var need []peer.Role
	if cfg.Storage.Role != chconfig.StorageRoleDevice {
		need = append(need, peer.RoleAPI)
	}
	if cfg.Storage.Role != chconfig.StorageRoleAPI {
		need = append(need, peer.RoleDevice)
	}
	for _, r := range need {
		if !id.Roles[r] {
			return nil, fmt.Errorf("[cluster] cert_file has no %q role, which [storage] role = %q needs", r, cfg.Storage.Role)
		}
	}
	return auth, nil
}

// newClusterProcess sets up opts for the cluster: the session registry for a process with a client listener and,
// with peer calls on, the cluster dispatcher (several client listener nodes can then share the database, and api
// processes claim scheduled runs, since they reach every client).
func newClusterProcess(cfg *chconfig.Config, db *sqlx.DB, coord *Coordinator, opts *chserver.ServerOpts) (*clusterProcess, error) {
	registry := clientsessions.New(db)
	p := &clusterProcess{
		cfg:     cfg,
		log:     logger.NewLogger("cluster-peer", cfg.Logging.LogOutput, cfg.Logging.LogLevel),
		coord:   coord,
		clients: pgclients.New(db, nil),
	}
	hasListener := cfg.Storage.Role != chconfig.StorageRoleAPI
	if hasListener {
		p.tracker = peer.NewTracker(registry, peer.NodeInfo{
			NodeID: cfg.Cluster.NodeID, Region: cfg.Cluster.Region, PeerAddress: cfg.Cluster.PeerAddress,
		}, 0, 0, p.log.Fork("sessions"))
		opts.ClientSessions = p.tracker
	}
	if !cfg.Cluster.PeerEnabled() {
		return p, nil
	}
	auth, err := peerAuth(cfg)
	if err != nil {
		return nil, err
	}
	p.auth = auth
	if cfg.Storage.Role != chconfig.StorageRoleDevice {
		p.dispatcher = peer.NewDispatcher(registry, peer.NewClient(auth), cfg.Cluster.NodeID, p.log.Fork("dispatch"))
		opts.Dispatcher = p.dispatcher
	}
	opts.TunnelHostInTunnels = hasListener
	return p, nil
}

func (p *clusterProcess) Bind(b chserver.PeerBinding) error {
	if p.dispatcher != nil {
		repo := b.Clients.GetRepo()
		p.dispatcher.Bind(peer.Binding{
			Local:             b.Local,
			HasClientListener: b.HasClientListener,
			UIJobStreams:      b.UIJobStreams,
			JobsDone:          b.JobsDone,
			// the change feed brings the tunnels too; refreshing at once answers the next request right
			Refresh: repo.Refresh,
		})
	}
	if p.auth != nil && b.HasClientListener {
		s, err := peer.NewServer(peer.ServerOptions{
			Auth:         p.auth,
			Local:        b.Local,
			UIJobStreams: b.UIJobStreams,
			UploadDir:    filepath.Join(p.cfg.GetUploadDir(), "peer"),
			Log:          p.log.Fork("listener"),
		})
		if err != nil {
			return err
		}
		p.server = s
	}
	return nil
}

func (p *clusterProcess) Start(ctx context.Context) error {
	if p.server != nil {
		if err := p.server.Start(p.cfg.Cluster.PeerListenAddress()); err != nil {
			return err
		}
	}
	ctx, p.cancel = context.WithCancel(ctx)
	if p.tracker != nil {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.tracker.Run(ctx)
		}()
	}
	if p.cfg.Storage.Role != chconfig.StorageRoleDevice {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.sweep(ctx)
		}()
	}
	return nil
}

// PeerAddr is the address the peer listener listens on, "" without one.
func (p *clusterProcess) PeerAddr() string {
	if p.server == nil {
		return ""
	}
	return p.server.Addr()
}

func (p *clusterProcess) Close() error {
	if p.cancel != nil {
		p.cancel()
	}
	var err error
	if p.server != nil {
		err = p.server.Close()
	}
	p.wg.Wait()
	return err
}

// sweep marks the clients disconnected whose session ended without the node saying so (it stopped, or lost the
// database long enough for the lease to run out).
func (p *clusterProcess) sweep(ctx context.Context) {
	tick := time.NewTicker(sweepInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		tenants, err := p.coord.Tenants(ctx)
		if err != nil {
			p.log.Errorf("session sweep: %v", err)
			continue
		}
		for _, tid := range tenants {
			ok, err := p.coord.Lease(ctx, "client-session-sweep", tid, 2*sweepInterval)
			if err != nil || !ok {
				continue
			}
			n, err := p.clients.MarkLostSessionsDisconnected(tenant.WithID(ctx, tid))
			switch {
			case err != nil && !errors.Is(err, context.Canceled):
				p.log.Errorf("session sweep of tenant %s: %v", tid, err)
			case n > 0:
				p.log.Infof("tenant %s: %d clients without a live session marked disconnected", tid, n)
			}
		}
	}
}
