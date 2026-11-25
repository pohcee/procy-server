package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/openrport/openrport/server/clients/clienttunnel"
	"github.com/openrport/openrport/server/dispatch"
	"github.com/openrport/openrport/server/jobrunner"
	"github.com/openrport/openrport/share/comm"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/tenant"
	"github.com/openrport/openrport/share/ws"
)

// Dispatcher is the dispatch.Dispatcher of a process in a cluster: it finds the node holding the client in the
// registry and passes the work to that node's device process over a peer call. When this process itself holds the
// client's connection (storage role "all"), the work is done locally.
type Dispatcher struct {
	locator Locator
	client  *Client
	nodeID  string
	log     *logger.Logger

	mu sync.RWMutex
	// set by Bind once the server is built
	local    dispatch.Dispatcher
	hasLocal bool
	streams  *ws.WebSocketCache
	done     *jobrunner.DoneChannels
	// refresh, when set, re-reads a client's record after its tunnels changed on another node
	refresh func(ctx context.Context, clientID string) error
	jobs    JobReader
}

// JobReader reads a stored job (tenant from ctx).
type JobReader interface {
	GetByJID(ctx context.Context, clientID, jid string) (*models.Job, error)
}

var _ dispatch.Dispatcher = (*Dispatcher)(nil)

// NewDispatcher returns a cluster dispatcher for the process of node nodeID.
func NewDispatcher(locator Locator, client *Client, nodeID string, log *logger.Logger) *Dispatcher {
	if log == nil {
		log = logger.NewLogger("peer", logger.LogOutput{}, logger.LogLevelError)
	}
	return &Dispatcher{locator: locator, client: client, nodeID: nodeID, log: log}
}

// Binding is what the dispatcher needs from the server it works for.
type Binding struct {
	// Local is the server's local dispatcher; used when HasClientListener and the client is connected here.
	Local             dispatch.Dispatcher
	HasClientListener bool
	// UIJobStreams and JobsDone get the output and results of jobs run on other nodes.
	UIJobStreams *ws.WebSocketCache
	JobsDone     *jobrunner.DoneChannels
	// Refresh re-reads a client's record (tenant in ctx); nil if the cache follows the change feed only.
	Refresh func(ctx context.Context, clientID string) error
	// Jobs reads the results the device processes save, for a run whose result didn't come over the stream.
	Jobs JobReader
}

// Bind connects the dispatcher to the server. Call it before the server runs.
func (d *Dispatcher) Bind(b Binding) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.local, d.hasLocal = b.Local, b.HasClientListener && b.Local != nil
	d.streams, d.done, d.refresh, d.jobs = b.UIJobStreams, b.JobsDone, b.Refresh, b.Jobs
}

// route returns the node holding the client, or the local dispatcher when it is this process.
func (d *Dispatcher) route(ctx context.Context, clientID string) (*Location, dispatch.Dispatcher, error) {
	if _, err := tenant.FromContext(ctx); err != nil {
		return nil, nil, err
	}
	loc, err := d.locator.Locate(ctx, clientID)
	if err != nil {
		return nil, nil, err
	}
	if loc == nil {
		return nil, nil, dispatch.ErrClientNotConnected
	}
	d.mu.RLock()
	local, hasLocal := d.local, d.hasLocal
	d.mu.RUnlock()
	if hasLocal && loc.NodeID == d.nodeID {
		return nil, local, nil
	}
	return loc, nil, nil
}

func targetOf(ctx context.Context, clientID string) target {
	tid, _ := tenant.FromContext(ctx)
	return target{Tenant: tid, ClientID: clientID}
}

func (d *Dispatcher) afterTunnelChange(ctx context.Context, clientID string) {
	d.mu.RLock()
	refresh := d.refresh
	d.mu.RUnlock()
	if refresh == nil {
		return
	}
	if err := refresh(ctx, clientID); err != nil {
		d.log.Errorf("refresh client %s after a tunnel change: %v", clientID, err)
	}
}

// RunJob starts the job on the node holding the client and keeps the peer stream open until the job's result
// arrives: output and the result are written to this process's UI job stream, and the result signals a sequential
// multi-client run waiting here. The result is also saved by the device process, so a broken stream loses only
// the live view.
func (d *Dispatcher) RunJob(ctx context.Context, clientID string, job *models.Job) (*comm.RunCmdResponse, error) {
	loc, local, err := d.route(ctx, clientID)
	if err != nil {
		return nil, err
	}
	if local != nil {
		return local.RunJob(ctx, clientID, job)
	}
	b, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	// the stream outlives the API request that started the job
	limit := time.Duration(job.TimeoutSec)*time.Second + runJobStreamGrace + time.Minute
	if job.TimeoutSec <= 0 {
		limit = time.Hour + time.Minute
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), limit)
	stream, err := d.client.runJob(sctx, loc, runJobRequest{target: targetOf(ctx, clientID), Job: b})
	if err != nil {
		cancel()
		return nil, err
	}
	first, err := stream.next()
	if err != nil {
		stream.close()
		cancel()
		return nil, fmt.Errorf("peer call to node %s: %w", loc.NodeID, err)
	}
	switch first.Type {
	case frameStarted:
	case frameError:
		stream.close()
		cancel()
		return nil, first.Error.err()
	default:
		stream.close()
		cancel()
		return nil, fmt.Errorf("peer call to node %s: unexpected frame %q", loc.NodeID, first.Type)
	}
	resp := &comm.RunCmdResponse{}
	if err := json.Unmarshal(first.Data, resp); err != nil {
		stream.close()
		cancel()
		return nil, err
	}
	key := job.JID
	if job.MultiJobID != nil {
		key = *job.MultiJobID
	}
	started := *job
	go func() {
		defer cancel()
		got := d.forward(key, stream)
		stream.close()
		if !got {
			d.awaitStoredResult(sctx, key, &started)
		}
	}()
	return resp, nil
}

// storedResultPoll is how often awaitStoredResult reads the job.
const storedResultPoll = 2 * time.Second

// awaitStoredResult ends what waits for job's result when it didn't come over the peer stream (the stream broke,
// the device process restarted): it reads the result the device process saved, until the stream's deadline (ctx),
// or else takes the job as failed, and writes it to the UI stream (its final write, so the stream ends) and gives it
// to a sequential multi-client run waiting for it.
func (d *Dispatcher) awaitStoredResult(ctx context.Context, key string, job *models.Job) {
	d.mu.RLock()
	streams, done, jobs := d.streams, d.done, d.jobs
	d.mu.RUnlock()
	var socket *ws.ConcurrentWebSocket
	if streams != nil {
		socket = streams.Get(key)
	}
	var ch chan *models.Job
	if done != nil && job.MultiJobID != nil {
		ch = done.Get(*job.MultiJobID)
	}
	if socket == nil && ch == nil {
		return
	}
	final := func() *models.Job {
		if jobs == nil {
			return nil
		}
		tick := time.NewTicker(storedResultPoll)
		defer tick.Stop()
		for {
			stored, err := jobs.GetByJID(ctx, job.ClientID, job.JID)
			if err == nil && stored != nil && stored.Status != models.JobStatusRunning {
				return stored
			}
			select {
			case <-ctx.Done():
				return nil
			case <-tick.C:
			}
		}
	}()
	if final == nil {
		failed := *job
		failed.Status = models.JobStatusUnknown
		failed.Error = "the result did not arrive from the node holding the client"
		final = &failed
	}
	if socket != nil {
		if b, err := json.Marshal(final); err == nil {
			if err := socket.WriteMessage(websocket.TextMessage, b); err != nil && !errors.Is(err, ws.ErrClosing) {
				d.log.Errorf("job %s: failed to write the result to the UI websocket: %v", key, err)
			}
		}
	}
	if ch != nil {
		go func() { ch <- final }()
	}
}

// forward copies the frames of a run-job stream to the UI and the waiting run; it reports whether the result came.
func (d *Dispatcher) forward(key string, stream *jobStream) bool {
	d.mu.RLock()
	streams, done := d.streams, d.done
	d.mu.RUnlock()
	for {
		f, err := stream.next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				d.log.Debugf("job stream %s ended: %v", key, err)
			}
			return false
		}
		var socket *ws.ConcurrentWebSocket
		if streams != nil {
			socket = streams.Get(key)
		}
		switch f.Type {
		case frameOutput:
			if socket != nil {
				_ = socket.WriteNonFinalJSON(f.Data)
			}
		case frameResult:
			if socket != nil {
				if err := socket.WriteMessage(websocket.TextMessage, f.Data); err != nil && !errors.Is(err, ws.ErrClosing) {
					d.log.Errorf("job %s: failed to write the result to the UI websocket: %v", key, err)
				}
			}
			var job models.Job
			if err := json.Unmarshal(f.Data, &job); err == nil && job.MultiJobID != nil && done != nil {
				if ch := done.Get(*job.MultiJobID); ch != nil {
					go func() { ch <- &job }()
				}
			}
			return true
		}
	}
}

// Upload streams the file to the node holding the client, which keeps it while the client fetches it.
func (d *Dispatcher) Upload(ctx context.Context, clientID string, file *models.UploadedFile) (*models.UploadResponse, error) {
	loc, local, err := d.route(ctx, clientID)
	if err != nil {
		return nil, err
	}
	if local != nil {
		return local.Upload(ctx, clientID, file)
	}
	f, err := os.Open(file.SourceFilePath)
	if err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	defer f.Close()
	meta := *file
	meta.SourceFilePath = ""
	b, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	ur, err := d.client.upload(ctx, loc, uploadRequest{target: targetOf(ctx, clientID), File: b}, f)
	if err != nil {
		return nil, err
	}
	var resp *models.UploadResponse
	if ur.Response != nil {
		resp = &models.UploadResponse{}
		if err := json.Unmarshal(ur.Response, resp); err != nil {
			return nil, err
		}
	}
	if ur.Error != nil {
		return resp, ur.Error.err()
	}
	return resp, nil
}

func (d *Dispatcher) IsTunnelAllowed(ctx context.Context, clientID, remote string) (bool, error) {
	loc, local, err := d.route(ctx, clientID)
	if err != nil {
		return false, err
	}
	if local != nil {
		return local.IsTunnelAllowed(ctx, clientID, remote)
	}
	var ok bool
	err = d.client.call(ctx, loc, targetOf(ctx, clientID), opIsTunnelAllowed, remoteArgs{Remote: remote}, &ok)
	return ok, err
}

func (d *Dispatcher) CheckPort(ctx context.Context, clientID string, req *comm.CheckPortRequest) (*comm.CheckPortResponse, error) {
	loc, local, err := d.route(ctx, clientID)
	if err != nil {
		return nil, err
	}
	if local != nil {
		return local.CheckPort(ctx, clientID, req)
	}
	resp := &comm.CheckPortResponse{}
	if err := d.client.call(ctx, loc, targetOf(ctx, clientID), opCheckPort, req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (d *Dispatcher) StartTunnels(ctx context.Context, clientID string, remotes []*models.Remote) ([]*clienttunnel.Tunnel, error) {
	loc, local, err := d.route(ctx, clientID)
	if err != nil {
		return nil, err
	}
	if local != nil {
		return local.StartTunnels(ctx, clientID, remotes)
	}
	var tunnels []*clienttunnel.Tunnel
	if err := d.client.call(ctx, loc, targetOf(ctx, clientID), opStartTunnels, remotes, &tunnels); err != nil {
		return nil, err
	}
	d.afterTunnelChange(ctx, clientID)
	return tunnels, nil
}

func (d *Dispatcher) CloseTunnel(ctx context.Context, clientID, tunnelID string, force bool) error {
	loc, local, err := d.route(ctx, clientID)
	if err != nil {
		return err
	}
	if local != nil {
		return local.CloseTunnel(ctx, clientID, tunnelID, force)
	}
	err = d.client.call(ctx, loc, targetOf(ctx, clientID), opCloseTunnel, tunnelArgs{TunnelID: tunnelID, Force: force}, nil)
	if err == nil {
		d.afterTunnelChange(ctx, clientID)
	}
	return err
}

func (d *Dispatcher) SetTunnelACL(ctx context.Context, clientID, tunnelID string, acl *string) error {
	loc, local, err := d.route(ctx, clientID)
	if err != nil {
		return err
	}
	if local != nil {
		return local.SetTunnelACL(ctx, clientID, tunnelID, acl)
	}
	err = d.client.call(ctx, loc, targetOf(ctx, clientID), opSetTunnelACL, tunnelArgs{TunnelID: tunnelID, ACL: acl}, nil)
	if err == nil {
		d.afterTunnelChange(ctx, clientID)
	}
	return err
}

func (d *Dispatcher) RefreshUpdatesStatus(ctx context.Context, clientID string) error {
	loc, local, err := d.route(ctx, clientID)
	if err != nil {
		return err
	}
	if local != nil {
		return local.RefreshUpdatesStatus(ctx, clientID)
	}
	return d.client.call(ctx, loc, targetOf(ctx, clientID), opRefreshUpdatesStatus, nil, nil)
}

func (d *Dispatcher) UpdateAttributes(ctx context.Context, clientID string, attributes models.Attributes) error {
	loc, local, err := d.route(ctx, clientID)
	if err != nil {
		return err
	}
	if local != nil {
		return local.UpdateAttributes(ctx, clientID, attributes)
	}
	return d.client.call(ctx, loc, targetOf(ctx, clientID), opUpdateAttributes, attributes, nil)
}
