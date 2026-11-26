package clients

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/hashicorp/go-version"

	alertingcap "github.com/pohcee/procy-server/plus/capabilities/alerting"
	"github.com/pohcee/procy-server/plus/capabilities/alerting/transformers"
	licensecap "github.com/pohcee/procy-server/plus/capabilities/license"

	"github.com/pohcee/procy-server/server/acme"
	apiErrors "github.com/pohcee/procy-server/server/api/errors"
	"github.com/pohcee/procy-server/server/caddy"
	"github.com/pohcee/procy-server/server/cgroups"
	"github.com/pohcee/procy-server/server/clients/clientdata"
	"github.com/pohcee/procy-server/server/clients/clienttunnel"
	"github.com/pohcee/procy-server/server/metering"
	"github.com/pohcee/procy-server/server/ports"
	chshare "github.com/pohcee/procy-server/share"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/models"
	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/tenant"
)

type ClientService interface {
	SetPlusLicenseInfoCap(licensecap licensecap.CapabilityEx)
	SetPlusAlertingServiceCap(as alertingcap.Service)

	Count(ctx context.Context) int
	CountActive(ctx context.Context) int
	CountDisconnected(ctx context.Context) (int, error)
	GetByID(ctx context.Context, id string) (*clientdata.Client, error)
	GetActiveByID(ctx context.Context, id string) (*clientdata.Client, error)
	GetByGroups(ctx context.Context, groups []*cgroups.ClientGroup) ([]*clientdata.Client, error)
	GetClientsByTag(ctx context.Context, tags []string, operator string, allowDisconnected bool) (clients []*clientdata.Client, err error)
	GetAllByClientID(ctx context.Context, clientID string) []*clientdata.Client
	GetAll(ctx context.Context) []*clientdata.Client
	GetUserClients(ctx context.Context, groups []*cgroups.ClientGroup, user User) []*clientdata.Client
	GetFilteredUserClients(ctx context.Context, user User, filterOptions []query.FilterOption, groups []*cgroups.ClientGroup) ([]*clientdata.CalculatedClient, error)

	PopulateGroupsWithUserClients(ctx context.Context, groups []*cgroups.ClientGroup, user User)
	UpdateClientStatus(ctx context.Context)

	StartClient(
		ctx context.Context, clientAuthID, clientID string, sshConn ssh.Conn, connectedHost string, authMultiuseCreds bool,
		req *chshare.ConnectionRequest, clog *logger.Logger,
	) (*clientdata.Client, error)
	Terminate(ctx context.Context, client *clientdata.Client) error
	ForceDelete(ctx context.Context, client *clientdata.Client) error
	DeleteOffline(ctx context.Context, clientID string) error

	SetACL(ctx context.Context, clientID string, allowedUserGroups []string) error
	CheckClientAccess(ctx context.Context, clientID string, user User, groups []*cgroups.ClientGroup) error
	CheckClientsAccess(clients []*clientdata.Client, user User, groups []*cgroups.ClientGroup) error

	SetUpdatesStatus(ctx context.Context, clientID string, updatesStatus *models.UpdatesStatus) error
	SetLastHeartbeat(ctx context.Context, clientID string, heartbeat time.Time) error
	SetIPAddresses(ctx context.Context, clientID string, IPAddresses *models.IPAddresses) error

	GetRepo() *ClientRepository

	SetCaddyAPI(capi caddy.API)
	StartClientTunnels(ctx context.Context, client *clientdata.Client, remotes []*models.Remote) ([]*clienttunnel.Tunnel, error)
	StartTunnel(c *clientdata.Client, r *models.Remote, acl *clienttunnel.TunnelACL) (*clienttunnel.Tunnel, error)
	FindTunnel(c *clientdata.Client, id string) *clienttunnel.Tunnel
	FindTunnelByRemote(c *clientdata.Client, r *models.Remote) *clienttunnel.Tunnel
	TerminateTunnel(ctx context.Context, c *clientdata.Client, t *clienttunnel.Tunnel, force bool) error
	SetTunnelACL(ctx context.Context, c *clientdata.Client, t *clienttunnel.Tunnel, aclStr *string) error
}

type ClientServiceProvider struct {
	repo              *ClientRepository
	portDistributor   *ports.PortDistributor
	tunnelProxyConfig *clienttunnel.InternalTunnelProxyConfig
	caddyAPI          caddy.API
	// tunnelHostInTunnels: each tunnel records tunnel_host (several nodes, each with its own name)
	tunnelHostInTunnels bool
	logger              *logger.Logger
	acme                *acme.Acme
	alertingService     alertingcap.Service

	licensecap licensecap.CapabilityEx

	// meter counts the tunnels' bytes (usage metering); nil: off
	meter *metering.Meter
	// tunnelRates gives the bandwidth of each tunnel connection per tenant (billing limits); nil: unlimited
	tunnelRates TunnelRates

	mu sync.RWMutex
}

var OptionsSupportedFilters = map[string]bool{
	"id":                       true,
	"name":                     true,
	"os":                       true,
	"os_arch":                  true,
	"os_family":                true,
	"os_kernel":                true,
	"os_full_name":             true,
	"os_version":               true,
	"os_virtualization_system": true,
	"os_virtualization_role":   true,
	"cpu_family":               true,
	"cpu_model":                true,
	"cpu_model_name":           true,
	"cpu_vendor":               true,
	"num_cpus":                 true,
	"timezone":                 true,
	"hostname":                 true,
	"ipv4":                     true,
	"ipv6":                     true,
	"tags":                     true,
	"labels":                   true,
	"version":                  true,
	"address":                  true,
	"connected_host":           true,
	"client_auth_id":           true,
	"allowed_user_groups":      true,
	"groups":                   true,
	"connection_state":         true,
}

var OptionsSupportedSorts = map[string]bool{
	"id":       true,
	"name":     true,
	"os":       true,
	"hostname": true,
	"version":  true,
}

var OptionsSupportedFields = map[string]map[string]bool{
	"clients": {
		"id":                       true,
		"name":                     true,
		"os":                       true,
		"os_arch":                  true,
		"os_family":                true,
		"os_kernel":                true,
		"hostname":                 true,
		"ipv4":                     true,
		"ipv6":                     true,
		"tags":                     true,
		"labels":                   true,
		"version":                  true,
		"address":                  true,
		"connected_host":           true,
		"tunnels":                  true,
		"disconnected_at":          true,
		"last_heartbeat_at":        true,
		"connection_state":         true,
		"client_auth_id":           true,
		"os_full_name":             true,
		"os_version":               true,
		"os_virtualization_system": true,
		"os_virtualization_role":   true,
		"cpu_family":               true,
		"cpu_model":                true,
		"cpu_model_name":           true,
		"cpu_vendor":               true,
		"timezone":                 true,
		"num_cpus":                 true,
		"mem_total":                true,
		"allowed_user_groups":      true,
		"updates_status":           true,
		"ip_addresses":             true,
		"client_configuration":     true,
		"groups":                   true,
	},
}

var OptionsListDefaultFields = map[string][]string{
	"fields[clients]": {
		"id",
		"name",
		"hostname",
	},
}

// NewClientService returns a new instance of client service.
func NewClientService(
	tunnelProxyConfig *clienttunnel.InternalTunnelProxyConfig,
	portDistributor *ports.PortDistributor,
	repo *ClientRepository,
	logger *logger.Logger,
	acme *acme.Acme,
) *ClientServiceProvider {
	csp := &ClientServiceProvider{
		tunnelProxyConfig: tunnelProxyConfig,
		portDistributor:   portDistributor,
		repo:              repo,
		logger:            logger.Fork("client-service"),
		acme:              acme,
	}

	return csp
}

func InitClientService(
	ctx context.Context,
	tunnelProxyConfig *clienttunnel.InternalTunnelProxyConfig,
	portDistributor *ports.PortDistributor,
	store ClientStore,
	keepDisconnectedClients *time.Duration,
	logger *logger.Logger,
	acme *acme.Acme,
) (*ClientServiceProvider, error) {
	repo, err := InitClientRepository(ctx, store, keepDisconnectedClients, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to init Client Repository: %v", err)
	}

	return NewClientService(tunnelProxyConfig, portDistributor, repo, logger, acme), nil
}

func (s *ClientServiceProvider) SetPlusLicenseInfoCap(licensecap licensecap.CapabilityEx) {
	s.licensecap = licensecap
}

func (s *ClientServiceProvider) SetPlusAlertingServiceCap(as alertingcap.Service) {
	s.alertingService = as
	if s.alertingService != nil {
		repo := s.GetRepo()
		repo.SetPostSaveHandlerFn(s.SendClientUpdateToAlerting)
	}
}

func (s *ClientServiceProvider) SendClientUpdateToAlerting(cl *clientdata.Client) {
	// note that the transformer uses the client getters so no need for an explicit lock here
	clientupdate, err := transformers.TransformRportClientToClientUpdate(cl)
	if err != nil {
		s.log().Debugf("unable to transform client update for alerting service")
		return
	}
	err = s.alertingService.PutClientUpdate(clientupdate)
	if err != nil {
		s.log().Debugf("Failed to send client update to the alerting service")
		return
	}
}

func (s *ClientServiceProvider) GetMaxClients() (maxClients int) {
	if s.licensecap != nil {
		maxClients = s.licensecap.GetMaxClients()
	}
	return maxClients
}

func (s *ClientServiceProvider) UpdateClientStatus(ctx context.Context) {
	// only proceed if the plus manager is available and license info has been received
	if s.licensecap == nil || (s.licensecap != nil && !s.licensecap.LicenseInfoAvailable()) {
		return
	}

	s.log().Debugf("updating client status")

	clientList := s.repo.GetAllActiveClients(ctx)

	for i, client := range clientList {
		if i < s.GetMaxClients() {
			client.SetPaused(false, "")
		} else {
			client.SetPaused(true, clientdata.PausedDueToMaxClientsExceeded)
		}
	}
}

func (s *ClientServiceProvider) Count(ctx context.Context) int {
	return s.repo.Count(ctx)
}

func (s *ClientServiceProvider) CountActive(ctx context.Context) int {
	return s.repo.CountActive(ctx)
}

func (s *ClientServiceProvider) CountDisconnected(ctx context.Context) (int, error) {
	return s.repo.CountDisconnected(ctx)
}

func (s *ClientServiceProvider) GetByID(ctx context.Context, id string) (*clientdata.Client, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *ClientServiceProvider) GetActiveByID(ctx context.Context, id string) (*clientdata.Client, error) {
	return s.repo.GetActiveByID(ctx, id)
}

func (s *ClientServiceProvider) GetByGroups(ctx context.Context, groups []*cgroups.ClientGroup) ([]*clientdata.Client, error) {
	if len(groups) == 0 {
		return nil, nil
	}
	if _, err := tenantOf(ctx); err != nil {
		return nil, err
	}

	allClients := s.repo.GetAllClients(ctx)

	var res []*clientdata.Client
	for _, cur := range allClients {
		if cur.BelongsToOneOf(groups) {
			res = append(res, cur)
		}
	}
	return res, nil
}

func (s *ClientServiceProvider) GetClientsByTag(ctx context.Context, tags []string, operator string, allowDisconnected bool) (clients []*clientdata.Client, err error) {
	return s.repo.GetClientsByTag(ctx, tags, operator, allowDisconnected)
}

func (s *ClientServiceProvider) PopulateGroupsWithUserClients(ctx context.Context, groups []*cgroups.ClientGroup, user User) {
	availableClients := s.repo.GetUserClients(ctx, user, groups)
	for _, client := range availableClients {
		clientID := client.GetID()
		for _, curGroup := range groups {
			if client.BelongsTo(curGroup) {
				curGroup.ClientIDs = append(curGroup.ClientIDs, clientID)
			}
		}
	}
	for _, curGroup := range groups {
		sort.Strings(curGroup.ClientIDs)
	}
}

func (s *ClientServiceProvider) GetAllByClientID(ctx context.Context, clientID string) []*clientdata.Client {
	return s.repo.GetAllByClientAuthID(ctx, clientID)
}

func (s *ClientServiceProvider) GetAll(ctx context.Context) []*clientdata.Client {
	return s.repo.GetAllClients(ctx)
}

func (s *ClientServiceProvider) GetUserClients(ctx context.Context, groups []*cgroups.ClientGroup, user User) []*clientdata.Client {
	return s.repo.GetUserClients(ctx, user, groups)
}

func (s *ClientServiceProvider) GetFilteredUserClients(ctx context.Context, user User, filterOptions []query.FilterOption, groups []*cgroups.ClientGroup) ([]*clientdata.CalculatedClient, error) {
	return s.repo.GetFilteredUserClients(ctx, user, filterOptions, groups)
}

func (s *ClientServiceProvider) StartClient(
	ctx context.Context, clientAuthID, clientID string, sshConn ssh.Conn, connectedHost string, authMultiuseCreds bool,
	req *chshare.ConnectionRequest, clog *logger.Logger,
) (*clientdata.Client, error) {
	clog.Debugf("Starting client session: %s", clientID)
	repo := s.GetRepo()

	clientAddr := sshConn.RemoteAddr().String()
	clientHost, _, err := net.SplitHostPort(clientAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to get host for address %q: %v", clientAddr, err)
	}

	// if client id is in use, deny connection
	client, err := repo.GetByID(ctx, clientID)
	if err != nil {
		return nil, fmt.Errorf("failed to get client by id %q: %w", clientID, err)
	}

	// if found existing client
	if client != nil {
		clog.Debugf("found existing client %s", clientID)
		var sessionReUsed = false
		if req.SessionID != "" && req.SessionID == client.GetSessionID() {
			// Stored previous session id and the session id of the connection attempt are equal
			sessionReUsed = true
			clog.Debugf("resuming existing session %s for client %s [%s]", req.SessionID, client.GetName(), clientID)
		}

		if client.IsConnected() && !sessionReUsed {
			clog.Debugf("client is already connected:  %s", clientID)
			return nil, fmt.Errorf("client is already connected: %s [%s]", client.GetName(), clientID)
		}

		oldTunnels := getTunnelsToReestablish(getRemotes(client.GetTunnels()), req.Remotes)

		clientVersion, err := version.NewVersion(req.Version)
		if err != nil {
			return nil, fmt.Errorf("failed to determine client version: %v", err)
		}
		requiredVersion, _ := version.NewVersion("0.6.4")
		if clientVersion.GreaterThanOrEqual(requiredVersion) {
			oldTunnels, err = s.excludeNotAllowedTunnels(clog, oldTunnels, sshConn)
			if err != nil {
				return nil, fmt.Errorf("failed to filter tunnels: %v", err)
			}
		} else {
			clog.Infof("client %s (%s) version %s does not support 'tunnel_allowed' policies. Consider upgrading.", client.GetID(), client.GetName(), client.GetVersion())
		}

		clog.Infof("tunnels to create %d: %v", len(req.Remotes), req.Remotes)
		if len(oldTunnels) > 0 {
			clog.Infof("old tunnels to re-establish %d: %v", len(oldTunnels), oldTunnels)
			req.Remotes = append(req.Remotes, oldTunnels...)
		}
	}

	// check if client auth ID is already used by another client
	if !authMultiuseCreds && s.isClientAuthIDInUse(ctx, clientAuthID, clientID) {
		clog.Debugf("client auth ID is already in use: %s: %q: ", clientID, clientAuthID)
		return nil, fmt.Errorf("client auth ID is already in use: %q", clientAuthID)
	}

	client = clientdata.NewClientFromConnRequest(ctx, client, clientAuthID, clientID, req, clientHost, connectedHost, sshConn, clog)

	client.SetConnected()

	s.UpdateClientStatus(ctx)

	if !client.IsPaused() {
		_, err = s.startClientTunnels(client, req.Remotes, clog)

		if err != nil {
			return nil, err
		}
	}

	err = repo.Save(ctx, client)
	if err != nil {
		return nil, err
	}

	// TODO: (rs): should we keep this?
	totalClients := repo.GetAllActiveClients(ctx)
	s.log().Debugf("total clients = %d (last: %s)", len(totalClients), client.GetName())

	return client, nil
}

func getRemotes(tunnels []*clienttunnel.Tunnel) []*models.Remote {
	r := make([]*models.Remote, 0, len(tunnels))
	for _, t := range tunnels {
		r = append(r, &t.Remote)
	}
	return r
}

// getTunnelsToReestablish returns old tunnels that should be re-establish taking into account new tunnels.
func getTunnelsToReestablish(old, updated []*models.Remote) []*models.Remote {
	if len(updated) > len(old) {
		return nil
	}

	// check if old tunnels contain all new tunnels
	// NOTE: old tunnels contain random port if local was not specified
	oldMarked := make([]bool, len(old))

	// at first check new with local specified. It's done at first to cover a case when a new tunnel was specified
	// with a port that is among random ports in old tunnels.
loop1:
	for _, curNew := range updated {
		if curNew.IsLocalSpecified() {
			for i, curOld := range old {
				if !oldMarked[i] && curNew.String() == curOld.String() {
					oldMarked[i] = true
					continue loop1
				}
			}
			return nil
		}
	}

	// then check without local
loop2:
	for _, curNew := range updated {
		if !curNew.IsLocalSpecified() {
			for i, curOld := range old {
				if !oldMarked[i] && curOld.LocalPortRandom && curNew.Remote() == curOld.Remote() && curNew.EqualACL(curOld.ACL) {
					oldMarked[i] = true
					continue loop2
				}
			}
			return nil
		}
	}

	// add tunnels that left among old
	var res []*models.Remote
	for i, marked := range oldMarked {
		if !marked {
			r := *old[i]
			// if it was random then set up zero values
			if r.LocalPortRandom {
				r.LocalHost = ""
				r.LocalPort = ""
			}
			res = append(res, &r)
		}
	}

	return res
}

// StartClientTunnels returns a new tunnel for each requested remote or nil if error occurred
func (s *ClientServiceProvider) StartClientTunnels(ctx context.Context, client *clientdata.Client, remotes []*models.Remote) ([]*clienttunnel.Tunnel, error) {
	s.logger.Debugf("starting client tunnels: %s", client.GetID())

	newTunnels, err := s.startClientTunnels(client, remotes, s.log())
	if err != nil {
		return nil, err
	}

	err = s.repo.Save(ctx, client)
	if err != nil {
		return nil, err
	}

	return newTunnels, err
}

func (s *ClientServiceProvider) startClientTunnels(client *clientdata.Client, remotes []*models.Remote, clog *logger.Logger) ([]*clienttunnel.Tunnel, error) {
	err := s.portDistributor.Refresh()
	if err != nil {
		return nil, err
	}

	tunnels := make([]*clienttunnel.Tunnel, 0, len(remotes))
	for _, remote := range remotes {
		if !remote.IsLocalSpecified() {
			clog.Debugf("no local specified")
			port, err := s.portDistributor.GetRandomPort(remote.Protocol)
			if err != nil {
				return nil, err
			}
			remote.LocalPort = strconv.Itoa(port)
			remote.LocalHost = models.ZeroHost
			remote.LocalPortRandom = true
			clog.Debugf("using random port %s", remote.LocalPort)
		} else {
			clog.Debugf("checking local port %s", remote.LocalPort)
			if err := s.checkLocalPort(remote.Protocol, remote.LocalPort); err != nil {
				return nil, err
			}
		}

		clog.Debugf("initiating tunnel %+v", remote)

		var acl *clienttunnel.TunnelACL
		if remote.ACL != nil {
			var err error
			acl, err = clienttunnel.ParseTunnelACL(*remote.ACL)
			if err != nil {
				return nil, err
			}
		}

		clog.Debugf("starting tunnel: %s", remote)
		t, err := s.StartTunnel(client, remote, acl)
		if err != nil {
			clog.Debugf("failed starting tunnel: %s: %v", remote, err)
			return nil, apiErrors.APIError{
				HTTPStatus: http.StatusConflict,
				Err:        fmt.Errorf("unable to start tunnel: %s", err),
			}
		}
		tunnels = append(tunnels, t)
	}

	return tunnels, nil
}

func (s *ClientServiceProvider) checkLocalPort(protocol, port string) error {
	localPort, err := strconv.Atoi(port)
	if err != nil {
		return apiErrors.NewAPIError(http.StatusBadRequest, "", fmt.Sprintf("Invalid local port: %s.", port), err)
	}

	if !s.portDistributor.IsPortAllowed(localPort) {
		return apiErrors.NewAPIError(http.StatusBadRequest, "", fmt.Sprintf("Local port %d is not among allowed ports.", localPort), nil)
	}

	if s.portDistributor.IsPortBusy(protocol, localPort) {
		return apiErrors.NewAPIError(http.StatusConflict, "", fmt.Sprintf("Local port %d already in use.", localPort), nil)
	}

	return nil
}

func (s *ClientServiceProvider) Terminate(ctx context.Context, client *clientdata.Client) error {
	s.log().Infof("terminating client: %s: %s", client.GetID(), client.GetName())
	keepDisconnectedClientsDuration := s.repo.GetKeepDisconnectedClients()
	if keepDisconnectedClientsDuration != nil && *keepDisconnectedClientsDuration == 0 {
		return s.repo.Delete(ctx, client)
	}

	client.SetDisconnectedNow()

	// Do not save if client doesn't exist in repo - it was force deleted
	existing, err := s.repo.GetByID(ctx, client.GetID())
	if err != nil {
		return err
	}
	if existing == nil {
		return nil
	}

	s.UpdateClientStatus(ctx)

	return s.repo.Save(ctx, client)
}

// ForceDelete deletes client from repo regardless off KeepDisconnectedClients setting,
// if client is active it will be closed
func (s *ClientServiceProvider) ForceDelete(ctx context.Context, client *clientdata.Client) error {
	s.logger.Debugf("force deleting client: %s", client.GetID())

	if client.IsConnected() {
		if err := client.Close(); err != nil {
			return err
		}
	}
	return s.repo.Delete(ctx, client)
}

func (s *ClientServiceProvider) DeleteOffline(ctx context.Context, clientID string) error {
	s.logger.Debugf("deleting offline client: %s", clientID)

	existing, err := s.getExistingClientByID(ctx, clientID)
	if err != nil {
		return err
	}

	if existing.IsConnected() {
		return apiErrors.APIError{
			Message:    "Client is active, should be disconnected",
			HTTPStatus: http.StatusBadRequest,
		}
	}

	return s.repo.Delete(ctx, existing)
}

// isClientAuthIDInUse returns true when the client with different id exists for the client auth
func (s *ClientServiceProvider) isClientAuthIDInUse(ctx context.Context, clientAuthID, clientID string) bool {
	for _, client := range s.repo.GetAllByClientAuthID(ctx, clientAuthID) {
		if client.GetID() != clientID {
			return true
		}
	}
	return false
}

func (s *ClientServiceProvider) SetACL(ctx context.Context, clientID string, allowedUserGroups []string) error {
	client, err := s.getExistingClientByID(ctx, clientID)
	if err != nil {
		return err
	}

	client.SetAllowedUserGroups(allowedUserGroups)

	return s.repo.SaveAdmin(ctx, client)
}

func (s *ClientServiceProvider) SetUpdatesStatus(ctx context.Context, clientID string, updatesStatus *models.UpdatesStatus) error {
	client, err := s.getExistingClientByID(ctx, clientID)
	if err != nil {
		return err
	}

	client.SetUpdatesStatus(updatesStatus)

	return s.repo.Save(ctx, client)
}

func (s *ClientServiceProvider) SetIPAddresses(ctx context.Context, clientID string, IPAddresses *models.IPAddresses) error {
	client, err := s.getExistingClientByID(ctx, clientID)
	if err != nil {
		return err
	}

	client.SetIPAddresses(IPAddresses)

	return s.repo.Save(ctx, client)
}

func (s *ClientServiceProvider) SetLastHeartbeat(ctx context.Context, clientID string, heartbeat time.Time) error {
	existing, err := s.getExistingClientByID(ctx, clientID)
	if err != nil {
		return err
	}
	existing.SetLastHeartbeatAt(&heartbeat)
	return nil
}

// CheckClientAccess returns nil if a given user has an access to a given client.
// Otherwise, APIError with 403 is returned.
func (s *ClientServiceProvider) CheckClientAccess(ctx context.Context, clientID string, user User, groups []*cgroups.ClientGroup) error {
	existing, err := s.getExistingClientByID(ctx, clientID)
	if err != nil {
		return err
	}

	return s.CheckClientsAccess([]*clientdata.Client{existing}, user, groups)
}

// CheckClientsAccess returns nil if a given user has an access to all of the given
// Otherwise, APIError with 403 is returned.
func (s *ClientServiceProvider) CheckClientsAccess(clients []*clientdata.Client, user User, clientGroups []*cgroups.ClientGroup) error {
	if user.IsAdmin() {
		return nil
	}

	var clientsWithNoAccess []string
	userGroups := user.GetGroups()
	for _, client := range clients {
		if client.HasAccessViaUserGroups(userGroups) || client.UserGroupHasAccessViaClientGroup(userGroups, clientGroups) {
			continue
		}

		clientsWithNoAccess = append(clientsWithNoAccess, client.GetID())
	}

	if len(clientsWithNoAccess) > 0 {
		return apiErrors.APIError{
			Message:    fmt.Sprintf("Access denied to client(s) with ID(s): %v", strings.Join(clientsWithNoAccess, ", ")),
			HTTPStatus: http.StatusForbidden,
		}
	}

	return nil
}

// getExistingClientByID returns non-nil client by id. If not found or failed to get a client - an error is returned.
func (s *ClientServiceProvider) getExistingClientByID(ctx context.Context, clientID string) (*clientdata.Client, error) {
	if clientID == "" {
		return nil, apiErrors.APIError{
			Message:    "Client id is empty",
			HTTPStatus: http.StatusBadRequest,
		}
	}

	existing, err := s.repo.GetByID(ctx, clientID)
	if err != nil {
		return nil, fmt.Errorf("failed to find a client with id=%q: %w", clientID, err)
	}

	if existing == nil {
		return nil, apiErrors.APIError{
			Message:    fmt.Sprintf("Client with id=%q not found.", clientID),
			HTTPStatus: http.StatusNotFound,
		}
	}

	return existing, nil
}

func (s *ClientServiceProvider) excludeNotAllowedTunnels(clog *logger.Logger, tunnels []*models.Remote, conn ssh.Conn) ([]*models.Remote, error) {
	filtered := make([]*models.Remote, 0, len(tunnels))
	for _, t := range tunnels {
		allowed, err := clienttunnel.IsAllowed(t.Remote(), conn, s.log())
		if err != nil {
			if strings.Contains(err.Error(), "unknown request") {
				return tunnels, nil
			}
			return nil, err
		}
		if !allowed {
			clog.Infof("Tunnel %q is no longer allowed by client config, removing.", t)
			continue
		}
		filtered = append(filtered, t)
	}
	return filtered, nil
}

// TODO: (rs): can this move to the tunnel package?
func (s *ClientServiceProvider) FindTunnelByRemote(c *clientdata.Client, r *models.Remote) *clienttunnel.Tunnel {
	for _, tunnel := range c.GetTunnels() {
		if tunnel.Equals(r) {
			return tunnel
		}
	}
	return nil
}

// TODO: (rs): can this move to the tunnel package?
func (s *ClientServiceProvider) FindTunnel(c *clientdata.Client, id string) *clienttunnel.Tunnel {
	for _, tunnel := range c.GetTunnels() {
		if tunnel.ID == id {
			return tunnel
		}
	}
	return nil
}

// SetTunnelHostInTunnels makes each tunnel started from now on carry [server] tunnel_host, the name of this server,
// so that links to it name the server holding it when several servers share the clients. Set during
// initialization.
// SetMeter counts the bytes of the tunnels started from now on in m (usage metering). Set during initialization.
func (s *ClientServiceProvider) SetMeter(m *metering.Meter) {
	s.meter = m
}

// TunnelRates gives the bandwidth of each tunnel connection of a tenant, in bytes per second in each direction
// (billing.Enforcer).
type TunnelRates interface {
	RateFunc(tid tenant.ID) func() int64
}

// SetTunnelRates limits the connections of the tunnels started from now on (billing limits). Set during
// initialization.
func (s *ClientServiceProvider) SetTunnelRates(r TunnelRates) {
	s.tunnelRates = r
}

// tunnelRate returns the rate of a new tunnel of client; nil: unlimited.
func (s *ClientServiceProvider) tunnelRate(client *clientdata.Client) clienttunnel.RateFunc {
	if s.tunnelRates == nil {
		return nil
	}
	tid, _ := tenant.FromContext(client.GetContext())
	if f := s.tunnelRates.RateFunc(tid); f != nil {
		return f
	}
	return nil
}

// tunnelBytes returns the byte counters of a new tunnel of client, in the client's tenant; nil without a meter.
func (s *ClientServiceProvider) tunnelBytes(client *clientdata.Client) *metering.TunnelBytes {
	if s.meter == nil {
		return nil
	}
	tid, _ := tenant.FromContext(client.GetContext())
	return s.meter.Tunnel(tid, client.GetID())
}

func (s *ClientServiceProvider) SetTunnelHostInTunnels(on bool) {
	s.tunnelHostInTunnels = on
}

func (s *ClientServiceProvider) SetCaddyAPI(capi caddy.API) {
	// unguarded as set during initialization
	s.caddyAPI = capi
}

func (s *ClientServiceProvider) StartTunnel(
	client *clientdata.Client,
	remote *models.Remote,
	acl *clienttunnel.TunnelACL) (tunnel *clienttunnel.Tunnel, err error) {
	tunnel = s.FindTunnelByRemote(client, remote)
	// tunnel exists
	if tunnel != nil {
		return tunnel, nil
	}

	s.log().Debugf("starting tunnel: %s", remote)

	ctx := client.GetContext()
	cancelAutoClose := context.CancelFunc(func() {})
	if remote.AutoClose > 0 {
		// ends with the client ctx or after the auto close period; on success
		// cleanupOnAutoCloseDeadlineExceeded releases it once it is done
		ctx, cancelAutoClose = context.WithTimeout(ctx, remote.AutoClose)
	}
	defer func() {
		if err != nil {
			cancelAutoClose()
		}
	}()

	startTunnelProxy := s.tunnelProxyConfig.Enabled && remote.HTTPProxy
	if startTunnelProxy {
		tunnel, err = s.startTunnelWithProxy(ctx, client, remote, acl)
		if err != nil {
			return nil, err
		}
		if remote.HasSubdomainTunnel() {
			err = s.startCaddyDownstreamProxy(ctx, client, remote, tunnel)
			if err != nil {
				tunnelStopErr := tunnel.InternalTunnelProxy.Stop(client.GetContext())
				if tunnelStopErr != nil {
					client.Log().Infof("unable to stop internal tunnel proxy after failing to create caddy downstream proxy: %s", tunnelStopErr)
				}
				return nil, err
			}
		}
	} else {
		tunnel, err = s.startRegularTunnel(ctx, client, remote, acl)
		if err != nil {
			return nil, err
		}
	}

	if s.tunnelHostInTunnels {
		tunnel.TunnelHost = s.tunnelProxyConfig.Host
	}

	// in case tunnel auto-closed due to auto close - run background task to remove the tunnel from the list
	// TODO: consider to create a separate background task to terminate all inactive tunnels based on some deadline/lastActivity time
	if tunnel.AutoClose > 0 {
		go s.cleanupOnAutoCloseDeadlineExceeded(ctx, cancelAutoClose, tunnel, client)
	}

	if tunnel.IdleTimeoutMinutes > 0 {
		go s.terminateTunnelOnIdleTimeout(ctx, tunnel, client)
	}

	existingTunnels := client.GetTunnels()
	existingTunnels = append(existingTunnels, tunnel)
	client.SetTunnels(existingTunnels)

	return tunnel, nil
}

func (s *ClientServiceProvider) startCaddyDownstreamProxy(
	ctx context.Context,
	client *clientdata.Client,
	remote *models.Remote,
	tunnel *clienttunnel.Tunnel,
) (err error) {
	clientLogger := client.Log()

	clientLogger.Infof("starting downstream caddy proxy at %s", remote.TunnelURL)
	clientLogger.Debugf("tunnel = %#v", tunnel)
	clientLogger.Debugf("remote = %#v", remote)

	subdomain, basedomain, err := remote.GetTunnelDomains()
	if err != nil {
		return err
	}

	nrr := &caddy.NewRouteRequest{
		RouteID:                   subdomain,
		TargetTunnelHost:          tunnel.LocalHost,
		TargetTunnelPort:          tunnel.LocalPort,
		DownstreamProxySubdomain:  subdomain,
		DownstreamProxyBaseDomain: basedomain,
	}

	clientLogger.Debugf("requesting new caddy route = %+v", nrr)

	res, err := s.caddyAPI.AddRoute(ctx, nrr)
	if err != nil {
		return err
	}

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to create downstream caddy proxy: status_code: %d", res.StatusCode)
	}

	clientLogger.Infof("started downstream caddy proxy at %s to %s:%s", remote.TunnelURL, tunnel.LocalHost, tunnel.LocalPort)
	return nil
}

func (s *ClientServiceProvider) startRegularTunnel(ctx context.Context, client *clientdata.Client, remote *models.Remote, acl *clienttunnel.TunnelACL) (*clienttunnel.Tunnel, error) {
	tunnelID := client.NewTunnelID()

	tunnel, err := clienttunnel.NewLimitedTunnel(client.Log(), client.GetConnection(), tunnelID, *remote, acl, s.tunnelBytes(client), s.tunnelRate(client))
	if err != nil {
		return nil, err
	}

	err = tunnel.Start(ctx)
	if err != nil {
		return nil, err
	}

	return tunnel, nil
}

func (s *ClientServiceProvider) startTunnelWithProxy(
	ctx context.Context,
	client *clientdata.Client,
	remote *models.Remote,
	acl *clienttunnel.TunnelACL,
) (*clienttunnel.Tunnel, error) {
	var proxyACL *clienttunnel.TunnelACL
	proxyHost := ""
	proxyPort := ""

	clientID := client.GetID()
	clientLogger := client.Log()

	// assuming that we still want to log activity in the client log
	clientLogger.Debugf("client %s will use tunnel proxy", clientID)

	// get values for tunnel proxy local host addr from original remote
	proxyHost = remote.LocalHost
	proxyPort = remote.LocalPort
	proxyACL = acl

	// reconfigure tunnel local host/addr to use 127.0.0.1 with a random port and make new acl
	remote.LocalHost = "127.0.0.1"
	port, err := s.portDistributor.GetRandomPort(remote.Protocol)
	if err != nil {
		return nil, err
	}

	remote.LocalPort = strconv.Itoa(port)
	acl, _ = clienttunnel.ParseTunnelACL("127.0.0.1") // access to tunnel is only allowed from localhost

	tunnelID := client.NewTunnelID()

	// original tunnel will use the reconfigured original remote
	t, err := clienttunnel.NewLimitedTunnel(clientLogger, client.GetConnection(), tunnelID, *remote, acl, s.tunnelBytes(client), s.tunnelRate(client))
	if err != nil {
		return nil, err
	}

	// start the original tunnel before the proxy tunnel
	err = t.Start(ctx)
	if err != nil {
		return nil, err
	}

	// create new proxy tunnel listening at the original tunnel local host addr
	tProxy := clienttunnel.NewInternalTunnelProxy(t, clientLogger, s.tunnelProxyConfig, proxyHost, proxyPort, proxyACL, s.acme)
	clientLogger.Debugf("client %s starting tunnel proxy", clientID)
	if err := tProxy.Start(ctx); err != nil {
		clientLogger.Debugf("tunnel proxy could not be started, tunnel must be terminated: %v", err)
		if tErr := t.Terminate(true); tErr != nil {
			return nil, tErr
		}
		return nil, fmt.Errorf("tunnel started and terminated because of tunnel proxy start error")
	}

	t.InternalTunnelProxy = tProxy

	// reconfigure original tunnel remote host addr to be the new proxy tunnel
	t.Remote.LocalHost = t.InternalTunnelProxy.Host
	t.Remote.LocalPort = t.InternalTunnelProxy.Port

	clientLogger.Debugf("client %s started tunnel with proxy: %#v", clientID, t)
	clientLogger.Debugf("internal tunnel proxy: %#v", t.InternalTunnelProxy)

	return t, nil
}

func (s *ClientServiceProvider) cleanupOnAutoCloseDeadlineExceeded(
	ctx context.Context,
	cancel context.CancelFunc,
	t *clienttunnel.Tunnel,
	c *clientdata.Client,
) {
	defer cancel()
	<-ctx.Done()
	// DeadlineExceeded err is expected when tunnel AutoClose period is reached, otherwise skip cleanup
	if ctx.Err() == context.DeadlineExceeded {
		s.cleanupAfterAutoClose(c, t)
	}
}

func (s *ClientServiceProvider) terminateTunnelOnIdleTimeout(ctx context.Context, t *clienttunnel.Tunnel, c *clientdata.Client) {
	idleTimeout := time.Duration(t.IdleTimeoutMinutes) * time.Minute
	timer := time.NewTimer(idleTimeout)
	for {
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			sinceLastActive := time.Since(t.LastActive())
			if sinceLastActive > idleTimeout {
				c.Log().Infof("Terminating... inactivity period is reached: %d minute(s)", t.IdleTimeoutMinutes)
				_ = t.Terminate(true)
				s.cleanupAfterAutoClose(c, t)
				return
			}
			timer.Reset(idleTimeout - sinceLastActive)
		}
	}
}

func (s *ClientServiceProvider) cleanupAfterAutoClose(c *clientdata.Client, t *clienttunnel.Tunnel) {
	clientLogger := c.Log()

	clientLogger.Infof("Auto closing tunnel %s ...", t.ID)

	// stop tunnel proxy
	if t.InternalTunnelProxy != nil {
		if err := t.InternalTunnelProxy.Stop(c.GetContext()); err != nil {
			clientLogger.Errorf("error while stopping tunnel proxy: %v", err)
		}
		if t.Remote.HasSubdomainTunnel() {
			_ = s.removeCaddyDownstreamProxy(c, t)
		}
	}

	c.RemoveTunnelByID(t.ID)

	// runs in a tunnel goroutine: the client's context carries its tenant
	err := s.repo.Save(c.GetContext(), c)
	if err != nil {
		clientLogger.Errorf("unable to save client after auto close cleanup: %v", err)
	}

	clientLogger.Debugf("auto closed tunnel with id=%s removed", t.ID)
}

func (s *ClientServiceProvider) TerminateTunnel(ctx context.Context, c *clientdata.Client, t *clienttunnel.Tunnel, force bool) error {
	clientLogger := c.Log()

	clientLogger.Infof("Terminating tunnel %s (force: %v) ...", t.ID, force)

	err := t.Terminate(force)
	if err != nil {
		return err
	}

	if t.InternalTunnelProxy != nil {
		if err := t.InternalTunnelProxy.Stop(c.GetContext()); err != nil {
			clientLogger.Errorf("error while stopping tunnel proxy: %v", err)
		}
		if t.Remote.HasSubdomainTunnel() {
			_ = s.removeCaddyDownstreamProxy(c, t)
		}
		if err != nil {
			return err
		}
	}

	c.RemoveTunnelByID(t.ID)

	err = s.repo.Save(ctx, c)
	if err != nil {
		clientLogger.Errorf("unable to save client after auto close cleanup: %v", err)
	}

	clientLogger.Debugf("terminated tunnel with id=%s removed", t.ID)
	return nil
}

func (s *ClientServiceProvider) SetTunnelACL(ctx context.Context, c *clientdata.Client, t *clienttunnel.Tunnel, aclStr *string) error {
	var err error
	var acl *clienttunnel.TunnelACL

	t.Remote.ACL = aclStr

	if aclStr != nil {
		acl, err = clienttunnel.ParseTunnelACL(*aclStr)
		if err != nil {
			return err
		}
	}
	t.TunnelProtocol.SetACL(acl)
	if t.InternalTunnelProxy != nil {
		t.InternalTunnelProxy.SetACL(acl)
	}

	err = s.repo.Save(ctx, c)
	if err != nil {
		c.Log().Errorf("unable to save client after tunnel ACL update: %v", err)
	}

	return nil
}

func (s *ClientServiceProvider) removeCaddyDownstreamProxy(c *clientdata.Client, t *clienttunnel.Tunnel) (err error) {
	clientLogger := c.Log()

	clientLogger.Infof("removing downstream caddy proxy at %s", t.Remote.TunnelURL)

	subdomain, _, err := t.Remote.GetTunnelDomains()
	if err != nil {
		return err
	}

	res, err := s.caddyAPI.DeleteRoute(c.GetContext(), subdomain)
	if err != nil {
		return err
	}

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to delete downstream caddy proxy: status_code: %d", res.StatusCode)
	}

	clientLogger.Infof("removed downstream caddy proxy at %s", t.Remote.TunnelURL)
	return nil
}

func (s *ClientServiceProvider) GetRepo() *ClientRepository {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repo
}

func (s *ClientServiceProvider) log() (l *logger.Logger) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.logger
}
