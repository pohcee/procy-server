package chserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jpillora/requestlog"
	"golang.org/x/crypto/ssh"

	rportplus "github.com/openrport/openrport/plus"
	alertingcap "github.com/openrport/openrport/plus/capabilities/alerting"
	"github.com/openrport/openrport/plus/capabilities/alerting/transformers"
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/api/middleware"
	"github.com/openrport/openrport/server/auditlog"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/server/clientsauth"
	"github.com/openrport/openrport/server/dispatch"
	"github.com/openrport/openrport/server/jobrunner"
	"github.com/openrport/openrport/server/metering"
	"github.com/openrport/openrport/server/monitoring"
	chshare "github.com/openrport/openrport/share"
	"github.com/openrport/openrport/share/comm"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/security"
	"github.com/openrport/openrport/share/tenant"
	"github.com/openrport/openrport/share/ws"
)

const (
	ConnectionRequestTimeOut = 5 * 60 * time.Second

	ClientRequestsLog     = "requests"
	ClientPingsLog        = "ping"
	ClientMeasurementsLog = "measurements"
)

var (
	ClientRequestsLogEnabled = true
	ClientPingsLogEnabled    = false
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(_ *http.Request) bool { return true },
}

// ClientListenerOptions are what the client listener needs from the rest of the server. Every
// field is required except AuditLog (nil: nothing is audited) and PlusManager (used only with
// rport-plus). In a process without the API and the job runner, give JobsDone and UIJobStreams
// empty values: job results are then saved and no sequential run or UI stream waits for them.
type ClientListenerOptions struct {
	Config *chconfig.Config
	// PrivateKey is the server's SSH host key.
	PrivateKey ssh.Signer
	// ClientService keeps the connected clients.
	ClientService clients.ClientService
	// ClientAuth checks the credentials clients connect with.
	ClientAuth clientsauth.Provider
	AuditLog   *auditlog.AuditLog
	// Jobs saves the job results clients send.
	Jobs         jobs.Provider
	JobsDone     *jobrunner.DoneChannels
	UIJobStreams *ws.WebSocketCache
	// FileGrants are the files clients may fetch over SFTP (granted by dispatch.Local uploads).
	FileGrants   *dispatch.FileGrants
	Measurements monitoring.MeasurementSaver
	PlusManager  rportplus.Manager
	// Capabilities are sent to every client that connects.
	Capabilities *models.Capabilities
	// Sessions, when set, records each connection in the cluster's session registry.
	Sessions dispatch.Sessions
	// DeviceTenants, when set, finds the tenant of each connecting device from its client auth id and decides
	// whether the tenant takes it (status, client limit). nil: every device belongs to the listener's tenant.
	DeviceTenants DeviceTenants
	// Meter, when set, counts the upload bytes the devices read (usage metering).
	Meter *metering.Meter
}

// listenerServices holds the options under the names the listener code uses.
type listenerServices struct {
	config             *chconfig.Config
	clientService      clients.ClientService
	clientAuthProvider clientsauth.Provider
	auditLog           *auditlog.AuditLog
	jobProvider        jobs.Provider
	jobsDoneChannel    *jobrunner.DoneChannels
	uiJobWebSockets    *ws.WebSocketCache
	fileGrants         *dispatch.FileGrants
	monitoringQueue    monitoring.MeasurementSaver
	plusManager        rportplus.Manager
	capabilities       *models.Capabilities
	sessions           dispatch.Sessions
	deviceTenants      DeviceTenants
	meter              *metering.Meter
}

type ClientListener struct {
	logger  *logger.Logger
	server  *listenerServices
	ctx     context.Context
	stopped atomic.Bool

	connStats         chshare.ConnStats
	httpServer        *chshare.HTTPServer
	reverseProxy      *httputil.ReverseProxy
	sshConfig         *ssh.ServerConfig
	requestLogOptions *requestlog.Options
	bannedClientAuths *security.BanList
	bannedIPs         *security.MaxBadAttemptsBanList

	clientIndexAutoIncrement int32

	// semaphore used to limit concurrent pending SSH connections
	inprogressSSHHandshakes chan struct{}

	mu sync.RWMutex
}

func (cl *ClientListener) log() (l *logger.Logger) {
	cl.mu.RLock()
	defer cl.mu.RUnlock()
	return cl.logger
}

func (cl *ClientListener) getCtx() (ctx context.Context) {
	cl.mu.RLock()
	defer cl.mu.RUnlock()
	return cl.ctx
}

func (cl *ClientListener) getClientService() (cs clients.ClientService) {
	cl.mu.RLock()
	defer cl.mu.RUnlock()
	return cl.server.clientService
}

// NewClientListener returns the listener clients connect to. It needs no API listener or job runner.
func NewClientListener(o ClientListenerOptions) (*ClientListener, error) {
	switch {
	case o.Config == nil:
		return nil, errors.New("client listener: Config is required")
	case o.PrivateKey == nil:
		return nil, errors.New("client listener: PrivateKey is required")
	case o.ClientService == nil:
		return nil, errors.New("client listener: ClientService is required")
	case o.ClientAuth == nil:
		return nil, errors.New("client listener: ClientAuth is required")
	case o.Jobs == nil:
		return nil, errors.New("client listener: Jobs is required")
	case o.JobsDone == nil:
		return nil, errors.New("client listener: JobsDone is required")
	case o.UIJobStreams == nil:
		return nil, errors.New("client listener: UIJobStreams is required")
	case o.FileGrants == nil:
		return nil, errors.New("client listener: FileGrants is required")
	case o.Measurements == nil:
		return nil, errors.New("client listener: Measurements is required")
	case o.Capabilities == nil:
		return nil, errors.New("client listener: Capabilities is required")
	}
	server := &listenerServices{
		config:             o.Config,
		clientService:      o.ClientService,
		clientAuthProvider: o.ClientAuth,
		auditLog:           o.AuditLog,
		jobProvider:        o.Jobs,
		jobsDoneChannel:    o.JobsDone,
		uiJobWebSockets:    o.UIJobStreams,
		fileGrants:         o.FileGrants,
		monitoringQueue:    o.Measurements,
		plusManager:        o.PlusManager,
		capabilities:       o.Capabilities,
		sessions:           o.Sessions,
		deviceTenants:      o.DeviceTenants,
		meter:              o.Meter,
	}
	privateKey := o.PrivateKey
	config := server.config

	// semaphore to limit number of active pending SSH connections
	inprogressSSHHandshakes := make(chan struct{}, config.Server.MaxConcurrentSSHConnectionHandshakes)

	clog := logger.NewLogger("client-listener", config.Logging.LogOutput, config.Logging.LogLevel)
	cl := &ClientListener{
		server:                  server,
		httpServer:              chshare.NewHTTPServer(int(config.Server.MaxRequestBytesClient), clog),
		requestLogOptions:       config.InitRequestLogOptions(),
		bannedClientAuths:       security.NewBanList(time.Duration(config.Server.ClientLoginWait) * time.Second),
		inprogressSSHHandshakes: inprogressSSHHandshakes,
		logger:                  clog,
	}

	if config.Server.MaxFailedLogin > 0 && config.Server.BanTime > 0 {
		cl.bannedIPs = security.NewMaxBadAttemptsBanList(
			config.Server.MaxFailedLogin,
			time.Duration(config.Server.BanTime)*time.Second,
			cl.logger,
		)
	}

	// create ssh config
	cl.sshConfig = &ssh.ServerConfig{
		ServerVersion:    "SSH-" + chshare.ProtocolVersion + "-server",
		PasswordCallback: cl.authUser,
	}

	cl.sshConfig.AddHostKey(privateKey)

	// setup reverse proxy
	if config.Server.Proxy != "" {
		u, err := url.Parse(config.Server.Proxy)
		if err != nil {
			return nil, err
		}
		if u.Host == "" {
			return nil, fmt.Errorf("missing protocol: %s", u)
		}
		cl.reverseProxy = httputil.NewSingleHostReverseProxy(u)
		// always use proxy host
		cl.reverseProxy.Director = func(r *http.Request) {
			r.URL.Scheme = u.Scheme
			r.URL.Host = u.Host
			r.Host = u.Host
		}
	}

	return cl, nil
}

// authUser is responsible for validating the ssh user / password combination
func (cl *ClientListener) authUser(c ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
	clientAuthID := c.User()

	if cl.bannedClientAuths.IsBanned(clientAuthID) {
		cl.log().Infof("Failed login attempt for client auth id %q, forcing to wait for %vs (%s)",
			clientAuthID,
			cl.server.config.Server.ClientLoginWait,
			cl.getIP(c.RemoteAddr()),
		)
		return nil, ErrTooManyRequests
	}

	// the listener context is the server context, so the credentials are looked up in its tenant, unless the
	// listener serves several tenants: then the client auth id names the tenant. Stored passwords are compared in
	// constant time, hashed ones (clusters) against their hash.
	ctx := cl.getCtx()
	var perms *ssh.Permissions
	ok := false
	tid, found, err := cl.tenantOfClientAuth(ctx, clientAuthID)
	if err != nil {
		cl.log().Errorf("Looking up the tenant of client auth id %q failed: %v", clientAuthID, err)
		return nil, err
	}
	if found {
		if tid != "" {
			ctx = tenant.WithID(ctx, tid)
			perms = &ssh.Permissions{Extensions: map[string]string{permTenant: string(tid)}}
		}
		ok, err = clientsauth.Verify(ctx, cl.server.clientAuthProvider, clientAuthID, password)
		if err != nil {
			cl.log().Errorf("Reading the credentials of client auth id %q failed: %v", clientAuthID, err)
			return nil, err
		}
	}

	ip := cl.getIP(c.RemoteAddr())
	if !ok {
		cl.log().Debugf("Login failed for client auth id: %s", clientAuthID)
		cl.bannedClientAuths.Add(clientAuthID)
		if cl.bannedIPs != nil {
			cl.bannedIPs.AddBadAttempt(ip)
		}
		return nil, fmt.Errorf("invalid authentication for client auth id: %s", clientAuthID)
	}

	if cl.bannedIPs != nil {
		cl.bannedIPs.AddSuccessAttempt(ip)
	}
	return perms, nil
}

// tenantOfClientAuth returns the tenant holding the client auth id when the listener serves several tenants, and
// whether one holds it; otherwise "" and true (the credentials are looked up in the listener's tenant).
func (cl *ClientListener) tenantOfClientAuth(ctx context.Context, clientAuthID string) (tenant.ID, bool, error) {
	if cl.server.deviceTenants == nil {
		return "", true, nil
	}
	tid, found, err := cl.server.deviceTenants.TenantOfClientAuth(ctx, clientAuthID)
	if err != nil {
		return "", false, err
	}
	return tid, found && tid != "", nil
}

func (cl *ClientListener) getIP(addr net.Addr) string {
	addrStr := addr.String()
	host, _, err := net.SplitHostPort(addrStr)
	if err != nil {
		cl.log().Errorf("failed to split host port for %q: %v", addr, err)
		return addrStr
	}
	return host
}

func (cl *ClientListener) Start(ctx context.Context, listenAddr string) error {
	clLogger := cl.log()
	clLogger.Debugf("Client listener starting...")

	// save the server ctx for use when handling client ssh connections etc
	cl.ctx = ctx
	cl.loadDeviceTenants(ctx)

	if cl.reverseProxy != nil {
		clLogger.Infof("Reverse proxy enabled")
	}
	clLogger.Infof("Listening on %s...", listenAddr)

	h := http.Handler(middleware.MaxBytes(http.HandlerFunc(cl.handleClient), cl.server.config.Server.MaxRequestBytesClient))
	if cl.bannedIPs != nil {
		h = security.RejectBannedIPs(cl.bannedIPs)(h)
	}
	h = requestlog.WrapWith(h, *cl.requestLogOptions)

	return cl.httpServer.GoListenAndServe(ctx, listenAddr, h)
}

// Wait waits for the http server to close
func (cl *ClientListener) Wait() error {
	cl.log().Debugf("client listener waiting...")
	err := cl.httpServer.Wait()
	cl.log().Debugf("client listener stopping...")
	cl.stopped.Store(true)
	return err
}

// Close forcibly closes the http server
func (cl *ClientListener) Close() error {
	err := cl.httpServer.Close()
	cl.stopped.Store(true)
	cl.log().Debugf("client listener stopped")
	return err
}

func (cl *ClientListener) handleClient(w http.ResponseWriter, r *http.Request) {
	cl.log().Debugf("Incoming client connection...")
	// websockets upgrade AND has rport prefix
	upgrade := strings.ToLower(r.Header.Get("Upgrade"))
	protocol := r.Header.Get("Sec-WebSocket-Protocol")
	if upgrade == "websocket" && strings.HasPrefix(protocol, "rport-") {
		if protocol == chshare.ProtocolVersion {
			cl.handleWebsocket(w, r)
			return
		}
		// print into server logs and silently fall-through
		cl.log().Infof("ignored client connection using protocol '%s', expected '%s'",
			protocol, chshare.ProtocolVersion)
	}
	// proxy target was provided
	if cl.reverseProxy != nil {
		cl.reverseProxy.ServeHTTP(w, r)
		return
	}

	w.WriteHeader(404)
	_, _ = w.Write([]byte{})
}

func (cl *ClientListener) nextClientIndex() int32 {
	return atomic.AddInt32(&cl.clientIndexAutoIncrement, 1)
}

func (cl *ClientListener) acceptSSHConnection(w http.ResponseWriter, req *http.Request) (sshConn *ssh.ServerConn, chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request,
	clog *logger.DynamicLogger, err error) {

	// throttle concurrent connections
	// add to pending connections. will block if the chan is full
	cl.inprogressSSHHandshakes <- struct{}{}
	defer func() {
		// on handshake finished, remove from pending connections, which will allow another connection to take place
		<-cl.inprogressSSHHandshakes
	}()

	clog = logger.ForkToDynamicLogger(cl.log(), fmt.Sprintf("client#%d", cl.nextClientIndex()), true, false)
	clog.SetControl(ClientRequestsLog, ClientRequestsLogEnabled)
	clog.SetControl(ClientPingsLog, ClientPingsLogEnabled)

	clog.Debugf("Handling inbound web socket connection...")
	ts := time.Now()

	wsConn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		clog.Debugf("Failed to upgrade (%s)", err)
		return nil, nil, nil, nil, err
	}
	conn := chshare.NewWebSocketConn(wsConn)
	// perform SSH handshake on net.Conn
	clog.Debugf("SSH Handshaking...")
	sshConn, chans, reqs, err = ssh.NewServerConn(conn, cl.sshConfig)
	if err != nil {
		if strings.Contains(err.Error(), "unexpected EOF") {
			clog.Debugf("Failed to handshake (client closed connection? - %s) from %s", err, conn.RemoteAddr().String())
		} else {
			clog.Debugf("Failed to handshake (%s) from %s", err, conn.RemoteAddr().String())
		}
		return nil, nil, nil, nil, err
	}
	clog.Debugf("SSH Handshake finished after %s", time.Since(ts))

	return sshConn, chans, reqs, clog, err
}

func (cl *ClientListener) receiveClientConnectionRequest(sshConn *ssh.ServerConn, reqs <-chan *ssh.Request, clog *logger.DynamicLogger) (connRequest *chshare.ConnectionRequest, r *ssh.Request, err error) {
	pendingRequestTimer := time.NewTimer(ConnectionRequestTimeOut)

	select {
	case r = <-reqs:
		pendingRequestTimer.Stop()

	case <-cl.getCtx().Done():
		pendingRequestTimer.Stop()
		return nil, nil, cl.ctx.Err()

	case <-pendingRequestTimer.C:
		errMsg := fmt.Sprintf("connection request timeout exceeded %0.2f sec", ConnectionRequestTimeOut.Seconds())
		clog.Debugf(errMsg)
		closeErr := sshConn.Close()
		if closeErr != nil {
			clog.Debugf("error on SSH connection close: %s", closeErr)
		}
		return nil, nil, errors.New(errMsg)
	}

	// it seems that nil requests are possible, so return an error
	if r == nil {
		return nil, nil, errors.New("received nil request from client")
	}

	if r.Type != "new_connection" {
		return nil, nil, errors.New("expecting connection request")
	}

	if len(r.Payload) > int(cl.server.config.Server.MaxRequestBytesClient) {
		return nil, nil, fmt.Errorf("request data exceeds the limit of %d bytes, actual size: %d", cl.server.config.Server.MaxRequestBytesClient, len(r.Payload))
	}

	connRequest, err = chshare.DecodeConnectionRequest(r.Payload)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid connection request: %s", err)
	}

	return connRequest, r, nil
}

// handleWebsocket is responsible for handling the websocket connection
func (cl *ClientListener) handleWebsocket(w http.ResponseWriter, req *http.Request) {
	// keep the time from the initial client connection attempt
	ts1 := time.Now()

	sshConn, chans, reqs, clientLog, err := cl.acceptSSHConnection(w, req)
	if err != nil {
		return
	}

	// verify configuration
	clientLog.Debugf("Verifying configuration...")

	// first request to be received must be a connection request
	connRequest, r, err := cl.receiveClientConnectionRequest(sshConn, reqs, clientLog)
	if err != nil {
		cl.replyConnectionError(r, err)
		return
	}

	clientLog.Debugf("client version: %s", connRequest.Version)

	checkVersions(clientLog, connRequest.Version)

	// get the current client auth id
	clientAuthID := sshConn.User()

	clientID, err := cl.getClientID(connRequest.ID, cl.server.config, clientAuthID)
	if err != nil {
		cl.replyConnectionError(r, fmt.Errorf("could not get clientID: %s", err))
		return
	}

	// the device runs in its tenant: everything it does later (job results, measurements, tunnels) is the tenant's
	ctx := cl.getCtx()
	tid := deviceTenantOf(sshConn)
	if tid != "" {
		ctx = tenant.WithID(ctx, tid)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := cl.admitDevice(ctx, tid, clientID, clientAuthID, clientLog.GetLogger()); err != nil {
		cl.replyConnectionError(r, err)
		return
	}

	client, err := cl.getClientService().StartClient(ctx, clientAuthID, clientID, sshConn, clientHostname(req.Host), cl.server.config.Server.AuthMultiuseCreds, connRequest, clientLog.GetLogger())
	if err != nil {
		cl.replyConnectionError(r, err)
		return
	}
	clientLog.Debugf("Client service started for %s (%s) within %s", client.GetID(), client.GetName(), time.Since(ts1))
	// recorded once the client is admitted (a refused duplicate must not replace the session of the connected one);
	// renewing stores the client connected again should the cluster's sweep catch it in between
	endSession := cl.registerSession(ctx, clientID, sshConn, clientLog)
	defer endSession()

	ts2 := time.Now()

	cl.replyConnectionSuccess(r, connRequest.Remotes)
	cl.sendCapabilities(sshConn)
	// Now the client is fully connected and ready to create tunnels and execute command and scripts

	clientBanner := client.Banner()
	clientLog.Debugf("opened %s within %s", clientBanner, time.Since(ts2))

	// now run handler for other client requests and connections
	go cl.handleSSHRequests(client.GetContext(), clientLog, clientID, reqs)
	go cl.handleSSHChannels(sshConn, clientLog.GetLogger(), chans, cl.uploadCounter(ctx, clientID))

	// wait until we're disconnected from the client
	if err = sshConn.Wait(); err != nil {
		clientLog.Debugf("sshConn.Wait() error: %s", err)
	}
	clientLog.Debugf("close %s", clientBanner)

	// ended first: saving the client as disconnected must not hide a newer session on another node
	endSession()
	err = cl.getClientService().Terminate(ctx, client)
	if err != nil {
		cl.log().Errorf("could not terminate client: %s", err)
	}
}

// registerSession records the connection in the session registry (when the server has one) and returns what ends
// it; a newer connection of the client elsewhere closes this one.
func (cl *ClientListener) registerSession(ctx context.Context, clientID string, sshConn ssh.Conn, clientLog *logger.DynamicLogger) func() {
	if cl.server.sessions == nil {
		return func() {}
	}
	end, err := cl.server.sessions.Connected(ctx, clientID, func() { _ = sshConn.Close() })
	if err != nil {
		clientLog.Errorf("client session not recorded: %v", err)
		return func() {}
	}
	var once sync.Once
	return func() { once.Do(end) }
}

// checkVersions print if client and server versions dont match.
func checkVersions(log *logger.DynamicLogger, clientVersion string) {
	if clientVersion == chshare.BuildVersion {
		return
	}

	v := clientVersion
	if v == "" {
		v = "<unknown>"
	}

	log.Infof("Client version (%s) differs from server version (%s)", v, chshare.BuildVersion)
}

func (cl *ClientListener) getClientID(reqID string, config *chconfig.Config, clientAuthID string) (string, error) {
	if reqID != "" {
		return reqID, nil
	}

	// use client auth id as client id if proper configs are set
	if !config.Server.AuthMultiuseCreds && config.Server.EquateClientauthidClientid {
		return clientAuthID, nil
	}

	return clientdata.NewClientID()
}

func (cl *ClientListener) replyConnectionSuccess(r *ssh.Request, remotes []*models.Remote) {
	replyPayload, err := json.Marshal(remotes)
	if err != nil {
		cl.log().Errorf("can't encode success reply payload")
		cl.replyConnectionError(r, err)
		return
	}

	err = r.Reply(true, replyPayload)
	if err != nil {
		cl.log().Errorf("error during connection success reply: %w", err)
	}
}

func (cl *ClientListener) replyConnectionError(r *ssh.Request, err error) {
	if r == nil {
		cl.log().Errorf("failed to send connection reply with error due to nil request")
		return
	}
	if err == nil {
		cl.log().Debugf("sending connection reply with nil error: %s", r.Type)
		err = r.Reply(false, nil)
		if err != nil {
			cl.log().Errorf("error during connection nil error reply: %w", err)
		}
		return
	}
	err = r.Reply(false, []byte(err.Error()))
	if err != nil {
		cl.log().Errorf("error during connection error reply: %w", err)
	}
}

// handleSSHRequests serves the requests of a connected client. clientCtx is the client's context: work the device
// originates (measurements, audit entries) belongs to its tenant.
func (cl *ClientListener) handleSSHRequests(clientCtx context.Context, clientLog *logger.DynamicLogger, clientID string, reqs <-chan *ssh.Request) {
	clientService := cl.getClientService()

	for r := range reqs {
		if cl.stopped.Load() {
			clientLog.Debugf("client listener has been stopped. stopping requests handler for %s", clientID)
			break
		}

		if len(r.Payload) > int(cl.server.config.Server.MaxRequestBytesClient) {
			clientLog.Errorf("%s:request data exceeds the limit of %d bytes, actual size: %d", comm.RequestTypeSaveMeasurement, cl.server.config.Server.MaxRequestBytesClient, len(r.Payload))
			continue
		}

		clientLog.NDebugf(ClientRequestsLog, "received request: %s from %s", r.Type, clientID)

		// TODO: (rs): these case handlers should be refactored into individual handling fns
		switch r.Type {

		// we shouldn't be receiving this. it means the client didn't receive the server's reply
		// to the previous connection request, so ask the client to reconnect.
		case "new_connection":
			clientLog.Debugf("received connection request on existing connection. asking the client to reconnect.")
			// IMPORTANT: the client is checking for the word "reconnect" in reply errors
			cl.replyConnectionError(r, errors.New("unexpected connection request. please reconnect"))
			client, err := clientService.GetRepo().GetActiveByID(clientCtx, clientID)
			if err != nil {
				clientLog.Debugf("unable to get client: %v", err)
				continue
			}
			if client == nil {
				clientLog.Debugf("client not found: %v", err)
				continue
			}
			clientLog.Debugf("terminating client due for reconnect")
			err = clientService.Terminate(clientCtx, client)
			if err != nil {
				clientLog.Debugf("failed to terminate client due for reconnect: %v", err)
			}
			continue

		case comm.RequestTypePing:
			var ts time.Time
			if ClientPingsLogEnabled {
				clientLog.NDebugf(ClientPingsLog, "ping received from: %s", clientID)
				ts = time.Now().UTC()
			}
			// ts := time.Now()
			_ = r.Reply(true, nil)
			err := clientService.SetLastHeartbeat(clientCtx, clientID, time.Now())
			if err != nil {
				clientLog.Errorf("Failed to save heartbeat: %s", err)
				continue
			}
			if ClientPingsLogEnabled {
				clientLog.NDebugf(ClientPingsLog, "ping for: %s done in %s", clientID, time.Since(ts))
			}

		case comm.RequestTypeCmdResult:
			clientLog.Debugf("saving command result from: %s", clientID)
			var ts time.Time
			if ClientRequestsLogEnabled {
				ts = time.Now().UTC()
			}

			job, err := cl.saveCmdResult(clientCtx, r.Payload)
			if err != nil {
				clientLog.Errorf("Failed to save cmd result: %s", err)
				continue
			}
			clientLog.Debugf("%s, Command result saved successfully.", job.LogPrefix())

			var auditLogEntry *auditlog.Entry
			if job.IsScript {
				auditLogEntry = cl.server.auditLog.Entry(auditlog.ApplicationClientScript, auditlog.ActionExecuteDone)
			} else {
				auditLogEntry = cl.server.auditLog.Entry(auditlog.ApplicationClientCommand, auditlog.ActionExecuteDone)
			}
			if job.MultiJobID != nil {
				auditLogEntry.WithID(*job.MultiJobID)
			} else {
				auditLogEntry.WithID(job.JID)
			}
			auditLogEntry.
				WithContext(clientCtx).
				WithResponse(job).
				WithClientID(clientID).
				Save()

			if job.MultiJobID != nil {
				done := cl.server.jobsDoneChannel.Get(*job.MultiJobID)
				if done != nil {
					// to avoid blocking the exec - send job result in a new goroutine
					go func(done2 chan *models.Job, job2 *models.Job) {
						done2 <- job2
					}(done, job)
				}
			}
			if ClientRequestsLogEnabled {
				clientLog.NDebugf(ClientRequestsLog, "%s: command results request completed at %s in %s", clientID, time.Now().UTC(), time.Since(ts))
			}

		case comm.RequestTypeUpdatesStatus:
			clientLog.Debugf("setting updates status from: %s", clientID)
			var ts time.Time
			if ClientRequestsLogEnabled {
				ts = time.Now().UTC()
			}
			updatesStatus := &models.UpdatesStatus{}
			err := json.Unmarshal(r.Payload, updatesStatus)
			if err != nil {
				clientLog.Errorf("Failed to unmarshal updates status: %s", err)
				continue
			}
			err = clientService.SetUpdatesStatus(clientCtx, clientID, updatesStatus)
			if err != nil {
				clientLog.Errorf("Failed to save updates status: %s", err)
				continue
			}
			if ClientRequestsLogEnabled {
				clientLog.NDebugf(ClientRequestsLog, "%s: updates updated at %s in %s", clientID, time.Now().UTC(), time.Since(ts))
			}

		case comm.RequestTypeSaveMeasurement:
			// if server monitoring is disabled then do not save measurements even if received
			if !cl.server.config.Monitoring.Enabled {
				clientLog.Errorf("Received measurement when monitoring disabled. Measurement not saved.")
				continue
			}

			measurement := models.Measurement{}
			err := json.Unmarshal(r.Payload, &measurement)
			if err != nil {
				clientLog.Errorf("Failed to unmarshal save_measurement: %s", err)
				continue
			}

			measurement.ClientID = clientID
			measurement.Timestamp = time.Now().UTC()

			cl.server.monitoringQueue.Notify(clientCtx, measurement)

			if rportplus.IsPlusEnabled(cl.server.config.PlusConfig) {
				alertingCap := cl.server.plusManager.GetAlertingCapabilityEx()
				if alertingCap != nil {
					cl.sendMeasurementToAlertingService(alertingCap, &measurement, clientLog)
				}
			}
		case comm.RequestTypeIPAddresses:
			clientLog.Debugf("IP addresses update received from: %s, payload: %s", clientID, r.Payload)
			IPAddresses := &models.IPAddresses{}
			err := json.Unmarshal(r.Payload, IPAddresses)
			if err != nil {
				clientLog.Errorf("Failed to unmarshal IP addresses: %s", err)
				continue
			}
			IPAddresses.UpdatedAt = time.Now().UTC()
			err = clientService.SetIPAddresses(clientCtx, clientID, IPAddresses)
			if err != nil {
				clientLog.Errorf("Failed to save IPAddresses status: %s", err)
				continue
			}
		default:
			clientLog.Debugf("Unknown request: %s", r.Type)
		}
	}

	clientLog.Debugf("Client listener for %s stopped", clientID)
}

func (cl *ClientListener) sendMeasurementToAlertingService(
	alertingCap alertingcap.CapabilityEx,
	measurement *models.Measurement,
	clientLog *logger.DynamicLogger) {

	m, err := transformers.TransformRportMeasurementToMeasure(measurement)
	if err != nil {
		clientLog.Debugf("Failed to transform measurement: %v", err)
		return
	}

	as := alertingCap.GetService()

	err = as.PutMeasurement(m)
	if err != nil {
		clientLog.Debugf("Failed to send measurement to the alerting service: %v", err)
		return
	}
}

// saveCmdResult saves a job result sent by a client; ctx is the client's context, which carries
// its tenant.
func (cl *ClientListener) saveCmdResult(ctx context.Context, respBytes []byte) (*models.Job, error) {
	// The result must be saved even when the connection ended or the server is shutting down, so the
	// writes keep the client's tenant but not its cancellation.
	ctx = context.WithoutCancel(ctx)
	resp := models.Job{}
	err := json.Unmarshal(respBytes, &resp)
	if err != nil {
		return nil, fmt.Errorf("failed to decode cmd result request: %s", err)
	}

	// older clients don't echo the library fields back; keep the stored ones,
	// both in the saved job and in the message forwarded to the UI
	fwdBytes := respBytes
	if resp.LibraryEntryID == "" || resp.LibraryVersion == 0 || resp.OS == "" {
		if stored, getErr := cl.server.jobProvider.GetByJID(ctx, resp.ClientID, resp.JID); getErr == nil && stored != nil {
			if resp.LibraryEntryID == "" {
				resp.LibraryEntryID = stored.LibraryEntryID
			}
			if resp.LibraryVersion == 0 {
				resp.LibraryVersion = stored.LibraryVersion
			}
			if resp.OS == "" {
				resp.OS = stored.OS
			}
			if stored.LibraryEntryID != "" {
				// library jobs only: typed messages are forwarded untouched
				fwdBytes = withLibraryFields(respBytes, &resp)
			}
		}
	}

	var wsJID string
	if resp.MultiJobID != nil {
		wsJID = *resp.MultiJobID
	} else {
		wsJID = resp.JID
	}
	ws := cl.server.uiJobWebSockets.Get(wsJID)
	if ws != nil {
		err := ws.WriteMessage(websocket.TextMessage, fwdBytes)
		if err != nil {
			cl.log().Errorf("%s, failed to write message to UI Web Socket: %v", resp.LogPrefix(), err)
			// proceed further
		}
	} else {
		cl.log().Debugf("%s, WS conn not found when saving command result. No active listeners connected", resp.LogPrefix())
	}

	err = cl.server.jobProvider.SaveJob(ctx, &resp)
	if err != nil {
		return nil, fmt.Errorf("failed to save job result: %s", err)
	}

	return &resp, nil
}

// uploadCounter returns what counts the upload bytes clientID (of the tenant of ctx) reads over SFTP, or nil
// without usage metering.
func (cl *ClientListener) uploadCounter(ctx context.Context, clientID string) func(n int64) {
	m := cl.server.meter
	if m == nil {
		return nil
	}
	tid, _ := tenant.FromContext(ctx)
	return func(n int64) { m.Add(tid, clientID, metering.UploadBytesToDevices, n) }
}

func (cl *ClientListener) handleSSHChannels(sshConn ssh.Conn, clientLog *logger.Logger, chans <-chan ssh.NewChannel, uploaded func(n int64)) {
	for ch := range chans {
		ch := ch
		extraData := string(ch.ExtraData())
		stream, reqs, err := ch.Accept()
		if err != nil {
			clientLog.Debugf("Failed to accept stream: %s", err)
			continue
		}

		go func() {
			for req := range reqs {
				cl.handleReq(req, clientLog)
			}
		}()

		switch ch.ChannelType() {
		case "session":
			cl.handleSessionChannel(sshConn, stream, clientLog, uploaded)
		case models.ChannelStdout, models.ChannelStderr:
			go func() {
				err := cl.handleOutputChannel(ch.ChannelType(), ch.ExtraData(), clientLog, stream)
				if err != nil {
					clientLog.Errorf("Error handling output channel %s: %v", ch.ChannelType(), err)
				}
			}()
		default:
			// handle stream type
			connID := cl.connStats.New()
			go chshare.HandleTCPStream(clientLog.Fork("conn#%d", connID), &cl.connStats, stream, extraData)
		}
	}
}

type outputChannelData struct {
	JID        string            `json:"jid"`
	ClientID   string            `json:"client_id"`
	ClientName string            `json:"client_name"`
	Result     *models.JobResult `json:"result"`
}

func (cl *ClientListener) handleOutputChannel(typ string, jobData []byte, clientLog *logger.Logger, stream io.Reader) error {
	job := models.Job{}
	err := json.Unmarshal(jobData, &job)
	if err != nil {
		return err
	}

	var wsJID string
	if job.MultiJobID != nil {
		wsJID = *job.MultiJobID
	} else {
		wsJID = job.JID
	}

	ws := cl.server.uiJobWebSockets.Get(wsJID)

	ocd := outputChannelData{
		JID:        job.JID,
		ClientID:   job.ClientID,
		ClientName: job.ClientName,
	}

	data := make([]byte, 4096)
	for {
		n, err := stream.Read(data)
		if err != nil {
			if err == io.EOF {
				clientLog.Debugf("Output channel %s for %s stop: %v", typ, wsJID, err)
				break
			}
			return err
		}

		if ws != nil {
			switch typ {
			case models.ChannelStdout:
				ocd.Result = &models.JobResult{
					StdOut: string(data[:n]),
				}
			case models.ChannelStderr:
				ocd.Result = &models.JobResult{
					StdErr: string(data[:n]),
				}
			}
			err := ws.WriteNonFinalJSON(ocd)
			if err != nil {
				clientLog.Errorf("Failed to write message to UI Web Socket: %v", err)
				// proceed further
			}
		} else {
			clientLog.Debugf("WS conn not found handling output channel. No active listeners connected")
		}
	}
	return nil
}

func (cl *ClientListener) handleReq(req *ssh.Request, clientLog *logger.Logger) {
	ok := false
	switch req.Type {
	// https://datatracker.ietf.org/doc/html/draft-ietf-secsh-filexfer-02#section-2
	case "subsystem":
		if string(req.Payload[4:]) == "sftp" {
			ok = true
		}
	}
	if req.WantReply {
		err := req.Reply(ok, nil)
		if err != nil {
			clientLog.Errorf("Failed to send ssh reply: %v", err)
		}
	}
}

// handleSessionChannel serves SFTP to a client. The client can read only the files that are
// being sent to it over this connection (see sendFileToClient); everything else is refused.
// uploaded, when set, counts the bytes of the files the client reads (usage metering).
func (cl *ClientListener) handleSessionChannel(sshConn ssh.Conn, stream io.ReadWriteCloser, clientLog *logger.Logger, uploaded func(n int64)) {
	server := newDeviceFileServer(stream, func(path string) bool {
		return cl.server.fileGrants.Allowed(sshConn, path)
	}, uploaded)

	if err := server.Serve(); err == io.EOF {
		e := server.Close()
		if e != nil {
			clientLog.Errorf("failed to close sftp server: %v", e)
		}
		clientLog.Debugf("sftp client exited session.")
	} else if err != nil {
		clientLog.Errorf("sftp server completed with error: %v", err)
	}
}

func (cl *ClientListener) sendCapabilities(conn *ssh.ServerConn) {
	payload, err := json.Marshal(cl.server.capabilities)
	if err != nil {
		cl.log().Errorf("can't encode capabilities payload")
		return
	}

	if _, _, err = conn.SendRequest(comm.RequestTypePutCapabilities, false, payload); err != nil {
		cl.log().Errorf("can't send capabilities: %v", err)
	}
}

// withLibraryFields returns raw with the library fields of job set where raw
// lacks them. On any decoding problem raw is returned unchanged.
func withLibraryFields(raw []byte, job *models.Job) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	set := func(key string, v interface{}) {
		if cur, ok := m[key]; ok && string(cur) != "null" && string(cur) != `""` && string(cur) != "0" {
			return
		}
		if b, err := json.Marshal(v); err == nil {
			m[key] = b
		}
	}
	set("library_entry_id", job.LibraryEntryID)
	set("library_version", job.LibraryVersion)
	set("os", job.OS)
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}
