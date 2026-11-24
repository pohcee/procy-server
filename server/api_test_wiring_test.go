package chserver

import (
	"context"

	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/server/clients/clienttunnel"
	"github.com/openrport/openrport/server/dispatch"
	"github.com/openrport/openrport/server/jobrunner"
	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/ws"
)

// liveClients is al's client service as it is when a request is dispatched: tests often set or
// replace al.clientService after building the listener.
type liveClients struct{ al *APIListener }

func (c liveClients) GetByID(ctx context.Context, id string) (*clientdata.Client, error) {
	return c.al.clientService.GetByID(ctx, id)
}

func (c liveClients) StartClientTunnels(ctx context.Context, client *clientdata.Client, remotes []*models.Remote) ([]*clienttunnel.Tunnel, error) {
	return c.al.clientService.StartClientTunnels(ctx, client, remotes)
}

func (c liveClients) FindTunnel(client *clientdata.Client, id string) *clienttunnel.Tunnel {
	return c.al.clientService.FindTunnel(client, id)
}

func (c liveClients) TerminateTunnel(ctx context.Context, client *clientdata.Client, t *clienttunnel.Tunnel, force bool) error {
	return c.al.clientService.TerminateTunnel(ctx, client, t, force)
}

func (c liveClients) SetTunnelACL(ctx context.Context, client *clientdata.Client, t *clienttunnel.Tunnel, acl *string) error {
	return c.al.clientService.SetTunnelACL(ctx, client, t, acl)
}

// liveJobs is al's jobs provider as it is when a job is saved.
type liveJobs struct{ al *APIListener }

func (j liveJobs) CreateJob(ctx context.Context, job *models.Job) error {
	return j.al.jobProvider.CreateJob(ctx, job)
}

func (j liveJobs) SaveMultiJob(ctx context.Context, job *models.MultiJob) error {
	return j.al.jobProvider.SaveMultiJob(ctx, job)
}

// liveTargets resolves with al's client service and groups as they are when a run resolves.
type liveTargets struct{ al *APIListener }

func (t liveTargets) Ordered(ctx context.Context, clientIDs, groupIDs []string) ([]*clientdata.Client, int, error) {
	return t.al.targets().Ordered(ctx, clientIDs, groupIDs)
}

func (t liveTargets) ByTag(ctx context.Context, tags *models.JobClientTags) ([]*clientdata.Client, error) {
	return t.al.targets().ByTag(ctx, tags)
}

// liveLibrary is al's library manager as it is when a library run starts.
type liveLibrary struct{ al *APIListener }

func (l liveLibrary) ActiveForRun(ctx context.Context, id string) (*library.Entry, error) {
	return l.al.libraryManager.ActiveForRun(ctx, id)
}

// useLocalDispatcher gives a hand-built API listener the local dispatcher NewServer would give it
// (unless the test set one) and a job runner on the listener's fields. The runner follows the
// test's later changes to the jobs provider, client service and library, signals al.testDone after
// each multi-client run, and with insecureForTests does not wait for sequential results. Call it
// again after changing insecureForTests or config.
func useLocalDispatcher(al *APIListener) {
	if al.Server == nil {
		al.Server = &Server{}
	}
	if al.dispatcher == nil {
		grants := al.fileGrants
		if grants == nil {
			grants = dispatch.NewFileGrants()
		}
		al.dispatcher = dispatch.NewLocal(liveClients{al}, grants, testLog)
	}
	if al.jobsDoneChannel == nil {
		al.jobsDoneChannel = jobrunner.NewDoneChannels()
	}
	cfg := al.config
	if cfg == nil {
		cfg = &chconfig.Config{}
	}
	runner, err := jobrunner.New(jobrunner.Options{
		Jobs:       liveJobs{al},
		Dispatcher: al.dispatcher,
		Targets:    liveTargets{al},
		Library:    liveLibrary{al},
		Config:     cfg,
		Done:       al.jobsDoneChannel,
		Logger:     testLog,
		NewJobID:   func() (string, error) { return generateNewJobID() },
		OnRunDone: func() {
			if al.testDone != nil {
				al.testDone <- true
			}
		},
		NoSequentialWait: al.insecureForTests,
	})
	if err != nil {
		panic(err)
	}
	al.jobRunner = runner
}

// newUIStreams returns an empty UI job stream cache for a hand-built client listener.
func newUIStreams() *ws.WebSocketCache {
	c := ws.NewWebSocketCache()
	return &c
}

// listenerServicesOf is what NewServer gives the client listener from s.
func listenerServicesOf(s *Server) *listenerServices {
	return &listenerServices{
		config:             s.config,
		clientService:      s.clientService,
		clientAuthProvider: s.clientAuthProvider,
		auditLog:           s.auditLog,
		jobProvider:        s.jobProvider,
		jobsDoneChannel:    s.jobsDoneChannel,
		uiJobWebSockets:    &s.uiJobWebSockets,
		fileGrants:         s.fileGrants,
		monitoringQueue:    s.monitoringQueue,
		plusManager:        s.plusManager,
		capabilities:       s.capabilities,
	}
}
