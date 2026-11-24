package chserver

import (
	"context"
	"errors"
	"fmt"
	"path"
	"runtime"
	"sync"
	"time"

	"go.etcd.io/bbolt"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"

	rportplus "github.com/openrport/openrport/plus"
	alertingcap "github.com/openrport/openrport/plus/capabilities/alerting"
	"github.com/openrport/openrport/server/acme"
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/api/jobs/schedule"
	"github.com/openrport/openrport/server/api/session"
	"github.com/openrport/openrport/server/auditlog"
	"github.com/openrport/openrport/server/caddy"
	servercaps "github.com/openrport/openrport/server/capabilities"
	"github.com/openrport/openrport/server/cgroups"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clientsauth"
	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/server/monitoring"
	"github.com/openrport/openrport/server/notifications"
	"github.com/openrport/openrport/server/ports"
	"github.com/openrport/openrport/server/scheduler"
	chshare "github.com/openrport/openrport/share"
	"github.com/openrport/openrport/share/capabilities"
	"github.com/openrport/openrport/share/files"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/tenant"
	"github.com/openrport/openrport/share/ws"
)

const (
	cleanupMeasurementsInterval  = time.Minute * 2
	cleanupAPISessionsInterval   = time.Hour
	cleanupJobsInterval          = time.Hour
	pruneLibraryVersionsInterval = 24 * time.Hour
	LogNumGoRoutinesInterval     = time.Minute * 2

	DefaultMaxClientDBConnections = 50

	// warning: do not increase this value without careful testing. bbolt doesn't seem to like too many concurrent
	// read/write workers.
	maxAlertingWorkers = 2
)

// Server represents a rport service
type Server struct {
	*logger.Logger
	clientListener      *ClientListener
	apiListener         *APIListener
	config              *chconfig.Config
	clientService       clients.ClientService
	clientAuthProvider  clientsauth.Provider
	jobProvider         JobProvider
	clientGroupProvider cgroups.ClientGroupProvider
	monitoringService   monitoring.Service
	// repos is the storage of every domain; storageClosers close the SQLite files no repository closes.
	repos            *Repositories
	storageClosers   []func() error
	uiJobWebSockets  ws.WebSocketCache // used to push job result to UI
	uploadWebSockets sync.Map
	jobsDoneChannel  jobResultChanMap // used for sequential command execution to know when command is finished
	auditLog         *auditlog.AuditLog
	capabilities     *models.Capabilities
	scheduleManager  *schedule.Manager
	filesAPI         files.FileAPI
	plusManager      rportplus.Manager
	caddyServer      *caddy.Server
	acme             *acme.Acme
	alertingService  alertingcap.Service
	monitoringQueue  monitoring.MeasurementSaver
	// capabilitiesOverride is ServerOpts.Capabilities; nil reports the computed capabilities.
	capabilitiesOverride func(servercaps.Capabilities) servercaps.Capabilities
}

type ServerOpts struct {
	FilesAPI    files.FileAPI
	PlusManager rportplus.Manager
	// Repositories, when set, replaces the storage of the domains whose field is set; nil fields (or a nil
	// Repositories) get the SQLite implementation in the data directory.
	Repositories *Repositories
	// SkipMigrations opens the SQLite files without migrating them and refuses to start unless every one is at the
	// latest version (run "rportd migrate" first). Files the server creates while running (the vault on init, the
	// audit log after a rotation) are migrated regardless.
	SkipMigrations bool
	// Capabilities, when set, adjusts the capabilities reported by GET /api/v1/status.
	// It receives the capabilities computed from the config and returns the ones to report.
	Capabilities func(servercaps.Capabilities) servercaps.Capabilities
}

// NewServer creates and returns a new rport server
func NewServer(ctx context.Context, config *chconfig.Config, opts *ServerOpts) (*Server, error) {
	var err error
	// Standalone: everything the server starts belongs to the single, invisible default tenant.
	ctx = tenant.WithID(ctx, tenant.Default)

	// X-Forwarded-For is only believed from these proxies; see share.RemoteIP.
	chshare.SetTrustedProxies(config.API.ParsedTrustedProxies)

	s := &Server{
		Logger:           logger.NewLogger("server", config.Logging.LogOutput, config.Logging.LogLevel),
		config:           config,
		uiJobWebSockets:  ws.NewWebSocketCache(),
		uploadWebSockets: sync.Map{},
		jobsDoneChannel: jobResultChanMap{
			m: make(map[string]chan *models.Job),
		},
	}

	s.acme = acme.New(s.Logger.Fork("acme"), config.Server.DataDir, config.Server.AcmeHTTPPort)
	if config.Server.InternalTunnelProxyConfig.EnableAcme {
		s.acme.AddHost(config.Server.InternalTunnelProxyConfig.Host)
	}

	filesAPI := opts.FilesAPI
	s.plusManager = opts.PlusManager
	s.capabilitiesOverride = opts.Capabilities

	if rportplus.IsPlusEnabled(config.PlusConfig) {
		licCap := s.plusManager.GetLicenseCapabilityEx()
		if licCap == nil {
			return nil, errors.New("failed to get license info capability from rport-plus")
		}

		licCap.SetLicenseInfoAvailableNotifier(s.HandlePlusLicenseInfoAvailable)

		alertingCap := s.plusManager.GetAlertingCapabilityEx()
		if alertingCap != nil {
			s.alertingService, err = s.StartPlusAlertingService(alertingCap, config.Server.DataDir)
			if err != nil {
				return nil, err
			}
			s.Infof("Alerting capability enabled")
		} else {
			s.Infof("Alerting capability not available")
		}
	}

	privateKey, err := initPrivateKey(config.Server.KeySeed)
	if err != nil {
		return nil, err
	}
	fingerprint := chshare.FingerprintKey(privateKey.PublicKey())
	s.Infof("Fingerprint %s", fingerprint)

	s.Infof("data directory path: %q", config.Server.DataDir)
	if config.Server.DataDir == "" {
		return nil, errors.New("data directory cannot be empty")
	}

	// create --data-dir path if not exist
	if makedirErr := filesAPI.MakeDirAll(config.Server.DataDir); makedirErr != nil {
		return nil, fmt.Errorf("failed to create data dir %q: %v", config.Server.DataDir, makedirErr)
	}

	// store fingerprint in file
	fingerprintFile := path.Join(config.Server.DataDir, "rportd-fingerprint.txt")
	if err := filesAPI.Write(fingerprintFile, fingerprint); err != nil {
		// juts log it and proceed
		s.Errorf("Failed to store fingerprint %q in file %q: %v", fingerprint, fingerprintFile, err)
	}

	repos, storage, err := openSQLiteRepositories(ctx, config, opts.Repositories, opts.SkipMigrations, s.Logger)
	if err != nil {
		return nil, err
	}
	s.repos = repos
	s.storageClosers = storage.shutdown

	s.jobProvider = repos.Jobs
	s.clientGroupProvider = repos.ClientGroups

	// even if monitoring disabled, always create the monitoring service to support queries of past data etc
	s.monitoringService = monitoring.NewService(repos.Monitoring, s.Logger.Fork("monitoring"))

	s.monitoringQueue = monitoring.NewMeasurementQueuing(ctx, s.Logger.Fork("measurements-queue"), s.monitoringService, 10000)

	s.clientService, err = clients.InitClientService(
		ctx,
		&s.config.Server.InternalTunnelProxyConfig,
		ports.NewPortDistributor(config.AllowedPorts()),
		repos.Clients,
		keepDisconnectedClients(config),
		s.Logger,
		s.acme,
	)
	if err != nil {
		return nil, err
	}

	if rportplus.IsPlusEnabled(config.PlusConfig) {
		licCapEx := s.plusManager.GetLicenseCapabilityEx()
		s.clientService.SetPlusLicenseInfoCap(licCapEx)
		s.clientService.SetPlusAlertingServiceCap(s.alertingService)
	}

	auditLogLogger := logger.NewLogger("auditlog", config.Logging.LogOutput, config.Logging.LogLevel)
	s.auditLog, err = auditlog.New(ctx, auditLogLogger, s.clientService, s.config.API.AuditLog, repos.AuditLog)
	if err != nil {
		return nil, err
	}

	s.clientAuthProvider = repos.ClientAuth

	s.clientListener, err = NewClientListener(s, privateKey)
	if err != nil {
		return nil, err
	}

	s.filesAPI = filesAPI

	s.apiListener, err = NewAPIListener(ctx, s, fingerprint)
	if err != nil {
		return nil, err
	}

	s.capabilities = capabilities.NewServerCapabilities(&config.Monitoring)

	s.scheduleManager, err = schedule.New(ctx, s.Logger, repos.Schedules, s.apiListener, config.Server.RunRemoteCmdTimeoutSec)
	if err != nil {
		return nil, err
	}
	s.scheduleManager.SetLibrary(s.apiListener.libraryManager, s.jobProvider)
	s.apiListener.libraryManager.SetReferenceChecker(s.scheduleManager)

	if s.config.CaddyEnabled() {
		cfg := s.config
		caddyLog := logger.NewLogger("caddy", cfg.Logging.LogOutput, cfg.Logging.LogLevel)

		baseConfig, err := cfg.WriteCaddyBaseConfig(&cfg.Caddy)
		if err != nil {
			return nil, err
		}

		caddy.HostDomainSocket = baseConfig.GlobalSettings.AdminSocket

		s.caddyServer = caddy.NewCaddyServer(&cfg.Caddy, caddyLog)
		s.clientService.SetCaddyAPI(s.caddyServer)
	}

	if s.alertingService != nil {
		tid, err := tenant.FromContext(ctx)
		if err != nil {
			return nil, err
		}
		// The alerting plugin may dispatch with a context of its own; it runs for the server's tenant.
		dispatcher := notifications.WithTenantFallback(notifications.NewDispatcher(s.apiListener.notificationsStorage), tid)
		s.alertingService.Run(ctx, config.Notifications.NotificationScriptDir, dispatcher, maxAlertingWorkers)
	}
	return s, nil
}

func (s *Server) HandlePlusLicenseInfoAvailable() {
	s.Logger.Debugf("received license info from rport-plus")

	if s.clientListener != nil && s.clientListener.server.clientService != nil {
		// the listener context is the server context; it is nil until the listener has started (no clients yet)
		if ctx := s.clientListener.getCtx(); ctx != nil {
			s.clientListener.server.clientService.UpdateClientStatus(ctx)
		}
	}
}

func (s *Server) StartPlusAlertingService(alertingCap alertingcap.CapabilityEx,
	dataDir string) (as alertingcap.Service, err error) {
	opts := bbolt.DefaultOptions
	// TODO: (rs):  previously the page size was being set in the wrong place. we still need to
	// performance test with larger page sizes, such as the below.
	// opts.PageSize = 1024 * 64
	bdb, err := bbolt.Open(dataDir+"/alerts.boltdb", 0600, opts)
	if err != nil {
		return nil, err
	}

	err = alertingCap.Init(bdb)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize the alerting service: %w", err)
	}

	as = alertingCap.GetService()

	err = as.LoadDefaultRuleSet()
	if err != nil {
		s.Infof("failed to load latest ruleset: %v", err)
	}

	return as, nil
}

// keepDisconnectedClients is how long disconnected clients are kept, or nil when they are kept forever (purging is
// disabled).
func keepDisconnectedClients(config *chconfig.Config) *time.Duration {
	if config.Server.PurgeDisconnectedClients {
		return &config.Server.KeepDisconnectedClients
	}
	return nil
}

func initPrivateKey(seed string) (ssh.Signer, error) {
	// generate private key (optionally using seed)
	key, err := chshare.GenerateKey(seed)
	if err != nil {
		return nil, fmt.Errorf("failed to generate key seed: %s", err)
	}
	// convert into ssh.PrivateKey
	private, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to parse key: %s", err)
	}
	return private, nil
}

// Run is responsible for starting the rport service
func (s *Server) Run(ctx context.Context) error {
	ctx = tenant.WithID(ctx, tenant.Default)
	if err := s.Start(ctx); err != nil {
		return err
	}

	s.acme.Start()

	// TODO(m-terel): add graceful shutdown of background task
	if s.config.Server.PurgeDisconnectedClients {
		s.Infof("Period to keep disconnected clients is set to %v", s.config.Server.KeepDisconnectedClients)
		go scheduler.Run(ctx, s.Logger, clients.NewCleanupTask(s.Logger, s.clientListener.server.clientService.GetRepo()), s.config.Server.PurgeDisconnectedClientsInterval)
		s.Infof("Task to purge disconnected clients will run with interval %v", s.config.Server.PurgeDisconnectedClientsInterval)
	} else {
		s.Debugf("Task to purge disconnected clients disabled")
	}

	// Run a task to Check the client connections status by sending and receiving pings
	clientsStatusCheckTask := NewClientsStatusCheckTask(
		s.Logger,
		s.clientListener.server.clientService.GetRepo(),
		s.config.Server.CheckClientsConnectionInterval,
		s.config.Server.CheckClientsConnectionTimeout,
	)
	go scheduler.Run(ctx, s.Logger.Fork(fmt.Sprintf("task %T", clientsStatusCheckTask)), clientsStatusCheckTask, s.config.Server.CheckClientsConnectionInterval)
	s.Infof("Task to check the clients connection status will run with interval %v", s.config.Server.CheckClientsConnectionInterval)

	if s.config.Monitoring.Enabled {
		var cleaningPeriod time.Duration
		if s.config.Monitoring.DataStorageDays > 0 {
			s.Infof("Period to keep measurements will be %d day(s)", s.config.Monitoring.DataStorageDays)
			cleaningPeriod = time.Hour * 24 * time.Duration(s.config.Monitoring.DataStorageDays)
		} else {
			s.Infof("Period to keep measurements will be %s", s.config.Monitoring.DataStorageDuration)
			cleaningPeriod = s.config.Monitoring.GetDataStorageDuration()
		}

		monitoringCleanupTask := monitoring.NewCleanupTask(s.Logger, s.monitoringService, cleaningPeriod)
		go scheduler.Run(ctx, s.Logger.Fork(fmt.Sprintf("task %T", monitoringCleanupTask)), monitoringCleanupTask, cleanupMeasurementsInterval)
		s.Infof("Task to cleanup measurements will run with interval %v", cleanupMeasurementsInterval)
	} else {
		s.Infof("Measurement disabled")
	}

	sessionsCleanupTask := session.NewCleanupTask(s.apiListener.apiSessions)
	go scheduler.Run(ctx, s.Logger.Fork(fmt.Sprintf("task %T", sessionsCleanupTask)), sessionsCleanupTask, cleanupAPISessionsInterval)
	s.Infof("Task to cleanup expired api sessions will run with interval %v", cleanupAPISessionsInterval)

	jobsCleanupTask := jobs.NewCleanupTask(s.jobProvider, s.config.Server.JobsMaxResults)
	go scheduler.Run(ctx, s.Logger.Fork(fmt.Sprintf("task %T", jobsCleanupTask)), jobsCleanupTask, cleanupJobsInterval)
	s.Infof("Task to cleanup jobs will run with interval %v", cleanupJobsInterval)

	if keep, days := s.config.Library.KeepVersions, s.config.Library.KeepVersionsDays; keep > 0 || days > 0 {
		libraryPruneTask := library.NewPruneTask(s.apiListener.libraryManager, keep, days, s.Logger)
		go scheduler.Run(ctx, s.Logger.Fork(fmt.Sprintf("task %T", libraryPruneTask)), libraryPruneTask, pruneLibraryVersionsInterval)
		s.Infof("Task to prune old library versions will run with interval %v", pruneLibraryVersionsInterval)
	}

	// Only on debug mode, log the number of running go routines
	if s.config.Logging.LogLevel == logger.LogLevelDebug {
		go func() {
			for {
				s.Logger.Debugf("Number of running go routines: %d", runtime.NumGoroutine())
				time.Sleep(LogNumGoRoutinesInterval)
			}
		}()
	}

	err := s.Wait()

	s.Debugf("stopping the server")

	// allow time for go-routines (and the caddy server) to process their cancellations
	time.Sleep(250 * time.Millisecond)

	s.Close()

	// a little more time for everything to settle on shutdown
	time.Sleep(500 * time.Millisecond)

	return err
}

// Start is responsible for kicking off the http server
func (s *Server) Start(ctx context.Context) error {
	s.Logger.Infof("will start server on %s", s.config.Server.ListenAddress)
	err := s.clientListener.Start(ctx, s.config.Server.ListenAddress)
	if err != nil {
		return err
	}

	if s.config.API.Address != "" {
		err = s.apiListener.Start(ctx, s.config.API.Address)
	}

	if s.config.CaddyEnabled() {
		err = s.caddyServer.Start(ctx)
	}

	return err
}

func (s *Server) Wait() error {
	wg := &errgroup.Group{}
	wg.Go(s.clientListener.Wait)
	wg.Go(s.apiListener.Wait)
	// if caddy configured then also set up a dependency on the caddy server running
	if s.config.CaddyEnabled() {
		wg.Go(s.caddyServer.Wait)
	}

	return wg.Wait()
}

func (s *Server) Close() error {
	s.Logger.Debugf("closing server")
	wg := &errgroup.Group{}

	wg.Go(s.clientListener.Close)
	wg.Go(s.apiListener.Close)
	if s.config.CaddyEnabled() {
		wg.Go(s.caddyServer.Close)
	}

	wg.Go(s.jobProvider.Close)

	wg.Go(s.clientGroupProvider.Close)
	wg.Go(s.uiJobWebSockets.CloseConnections)

	if s.auditLog != nil {
		wg.Go(s.auditLog.Close)
	}

	s.uploadWebSockets.Range(func(key, value interface{}) bool {
		if wsConn, ok := value.(*ws.ConcurrentWebSocket); ok {
			wg.Go(wsConn.Close)
		}
		return true
	})

	wg.Go(s.monitoringQueue.Close)

	// the SQLite files (and the [database] connection) that no repository closes
	for _, closeFn := range s.storageClosers {
		wg.Go(closeFn)
	}

	// TODO: (rs):  should we be shutting down the other plugin capabilities here?
	if s.alertingService != nil {
		s.Debugf("stopping alerting service")
		wg.Go(s.alertingService.Stop)
	}

	err := wg.Wait()
	if err != nil {
		s.Logger.Errorf("error closing server: %v", err)
	}

	return err
}

// jobResultChanMap is thread safe map with [jobID, chan *models.Job] pairs.
type jobResultChanMap struct {
	m  map[string]chan *models.Job
	mu sync.RWMutex
}

func (m *jobResultChanMap) Set(jobID string, done chan *models.Job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[jobID] = done
}

func (m *jobResultChanMap) Del(jobID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.m, jobID)
}

func (m *jobResultChanMap) Get(jobID string) chan *models.Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.m[jobID]
}

// TODO: only used for testing purposes. we should review the approach.
func (m *jobResultChanMap) GetAllKeys() (jobIDs []string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	jobIDs = make([]string, 0, len(m.m))
	for k := range m.m {
		jobIDs = append(jobIDs, k)
	}
	return jobIDs
}
