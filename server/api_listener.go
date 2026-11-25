package chserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/sync/errgroup"

	"github.com/gorilla/mux"
	"github.com/jpillora/requestlog"

	"github.com/openrport/openrport/server/acme"
	"github.com/openrport/openrport/server/auditlog"
	servercaps "github.com/openrport/openrport/server/capabilities"
	"github.com/openrport/openrport/server/cgroups"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clientsauth"
	"github.com/openrport/openrport/server/dispatch"
	"github.com/openrport/openrport/server/jobrunner"
	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/server/metering"
	"github.com/openrport/openrport/server/monitoring"
	"github.com/openrport/openrport/server/notifications"
	"github.com/openrport/openrport/server/notifications/channels/rmailer"
	"github.com/openrport/openrport/server/notifications/channels/scriptRunner"
	"github.com/openrport/openrport/server/notifications/channels/toLog"
	notificationsSQLite "github.com/openrport/openrport/server/notifications/repository/sqlite"
	"github.com/openrport/openrport/server/operator"
	"github.com/openrport/openrport/server/routes"

	"github.com/openrport/openrport/server/api/authorization"
	"github.com/openrport/openrport/server/api/jobs/schedule"
	"github.com/openrport/openrport/server/api/session"
	"github.com/openrport/openrport/server/api/uploads"
	"github.com/openrport/openrport/server/billing"
	"github.com/openrport/openrport/server/clients/storedtunnels"
	"github.com/openrport/openrport/server/pairing"

	rportplus "github.com/openrport/openrport/plus"
	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/api/message"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/bearer"
	"github.com/openrport/openrport/server/vault"

	extperm "github.com/openrport/openrport/plus/capabilities/extendedpermission"
	chshare "github.com/openrport/openrport/share"
	"github.com/openrport/openrport/share/enums"
	"github.com/openrport/openrport/share/files"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/random"
	"github.com/openrport/openrport/share/security"
	"github.com/openrport/openrport/share/tenant"
	"github.com/openrport/openrport/share/ws"
)

const (
	DefaultMaxCheckPortTimeout = time.Minute
)

// APIListenerOptions are what the API listener (the API and the web UI) needs from the rest of the
// server. Every field is required except Fingerprint, PlusManager (used only with rport-plus),
// Capabilities, and Acme (required only when [api] enable_acme is set).
type APIListenerOptions struct {
	Config *chconfig.Config
	// Fingerprint is the server's SSH host key fingerprint, shown to API users.
	Fingerprint string
	// Repositories is the storage; every field the API uses must be set.
	Repositories  *Repositories
	ClientService clients.ClientService
	// Dispatcher does the work on clients.
	Dispatcher dispatch.Dispatcher
	// JobRunner runs the commands, scripts and library entries the API starts.
	JobRunner *jobrunner.Runner
	Library   *library.Manager
	Schedules *schedule.Manager
	AuditLog  *auditlog.AuditLog
	// Monitoring answers the metrics queries.
	Monitoring  monitoring.Service
	Files       files.FileAPI
	PlusManager rportplus.Manager
	Acme        *acme.Acme
	// UIJobStreams are the UI websockets job results are streamed to; the client listener must get
	// the same cache.
	UIJobStreams *ws.WebSocketCache
	// Capabilities, when set, adjusts the capabilities reported by GET /api/v1/status.
	Capabilities func(servercaps.Capabilities) servercaps.Capabilities
	// TenantResolver resolves the tenant of each API request before authentication (#34); nil
	// means tenant.DefaultResolver (standalone).
	TenantResolver tenant.Resolver
	// VaultKeys gives the key that encrypts the vault values; nil: the passphrase given through the API.
	VaultKeys vault.KeyProvider
	// Operator, when set, serves the operator API; nil: no operator routes.
	Operator operator.Service
	// Usage, when set (usage metering on), serves GET /usage and the operator usage routes, and reports the usage
	// capability.
	Usage metering.Reader
	// Meter, when set, counts the bytes of the API responses (usage metering).
	Meter *metering.Meter
	// Billing, when set ([billing] enabled), serves GET /billing and the operator billing routes, refuses tunnels
	// and uploads at the hard transfer cap and reports the billing capability.
	Billing *BillingAPI
	// NoNotificationsCleaner leaves deleting old notifications to the server (per tenant, in a cluster).
	NoNotificationsCleaner bool
}

// apiServices holds the options under the names the handler code uses.
type apiServices struct {
	config               *chconfig.Config
	clientService        clients.ClientService
	clientAuthProvider   clientsauth.Provider
	jobProvider          JobProvider
	clientGroupProvider  cgroups.ClientGroupProvider
	monitoringService    monitoring.Service
	dispatcher           dispatch.Dispatcher
	jobRunner            *jobrunner.Runner
	libraryManager       *library.Manager
	scheduleManager      *schedule.Manager
	auditLog             *auditlog.AuditLog
	filesAPI             files.FileAPI
	plusManager          rportplus.Manager
	uiJobWebSockets      *ws.WebSocketCache
	capabilitiesOverride func(servercaps.Capabilities) servercaps.Capabilities
}

type APIListener struct {
	*logger.Logger
	errResponseLogger *logger.Logger

	*apiServices

	// uploads serves the upload routes; made with the router
	uploads *uploads.Handler
	// operatorAPI serves the operator routes; nil without APIListenerOptions.Operator
	operatorAPI *operator.Handler
	// usage reads the usage for GET /usage; nil: metering off
	usage metering.Reader
	// billing serves GET /billing; nil: billing off
	billing *BillingAPI
	// meter counts the API bytes; nil: metering off
	meter *metering.Meter

	fingerprint       string
	apiSessions       *session.Cache
	router            *mux.Router
	httpServer        *chshare.HTTPServer
	requestLogOptions *requestlog.Options
	accessLogFile     io.WriteCloser
	insecureForTests  bool
	// tenantResolver resolves the tenant of each API request; nil means tenant.DefaultResolver (standalone).
	tenantResolver tenant.Resolver
	bannedUsers    *security.BanList
	bannedIPs      *security.MaxBadAttemptsBanList
	twoFASrv       TwoFAService

	testDone chan bool // is used only in tests to be able to wait until async task is done

	userService   UserService
	vaultManager  *vault.Manager
	tokenManager  *authorization.Manager
	storedTunnels *storedtunnels.Manager
	pairing       *pairing.Service

	notificationsStorage   notifications.Repository
	notificationsProcessor notifications.Processor
	notificationsCleaner   notificationsSQLite.Closeable

	mu sync.RWMutex
}

func (al *APIListener) Log() (l *logger.Logger) {
	al.mu.RLock()
	defer al.mu.RUnlock()

	return al.Logger
}

type UserService interface {
	GetAll(ctx context.Context) ([]*users.User, error)
	GetByUsername(ctx context.Context, username string) (*users.User, error)
	Change(ctx context.Context, user *users.User, username string) error
	Delete(ctx context.Context, username string) error
	ExistGroups(ctx context.Context, groups []string) error
	GetProviderType() enums.ProviderSource
	ListGroups(ctx context.Context) ([]users.Group, error)
	GetGroup(ctx context.Context, name string) (users.Group, error)
	UpdateGroup(ctx context.Context, name string, g users.Group) (users.Group, error)
	DeleteGroup(ctx context.Context, name string) error
	CheckPermission(ctx context.Context, user *users.User, permission string) error
	SupportsGroupPermissions() bool
	GetEffectiveUserPermissions(ctx context.Context, user *users.User) (map[string]bool, error)
	GetEffectiveUserExtendedPermissions(ctx context.Context, user *users.User) ([]extperm.PermissionParams, []extperm.PermissionParams)
}

// NewAPIListener returns the API listener. It needs no client listener: work on clients goes
// through o.Dispatcher. ctx is the server context; it carries the tenant that startup work (the
// session cache) runs in.
func NewAPIListener(ctx context.Context, o APIListenerOptions) (*APIListener, error) {
	if _, err := tenant.FromContext(ctx); err != nil {
		return nil, fmt.Errorf("api listener: %w", err)
	}
	switch {
	case o.Config == nil:
		return nil, errors.New("api listener: Config is required")
	case o.Repositories == nil:
		return nil, errors.New("api listener: Repositories is required")
	case o.ClientService == nil:
		return nil, errors.New("api listener: ClientService is required")
	case o.Dispatcher == nil:
		return nil, errors.New("api listener: Dispatcher is required")
	case o.JobRunner == nil:
		return nil, errors.New("api listener: JobRunner is required")
	case o.Library == nil:
		return nil, errors.New("api listener: Library is required")
	case o.Schedules == nil:
		return nil, errors.New("api listener: Schedules is required")
	case o.AuditLog == nil:
		return nil, errors.New("api listener: AuditLog is required")
	case o.Files == nil:
		return nil, errors.New("api listener: Files is required")
	case o.Monitoring == nil:
		return nil, errors.New("api listener: Monitoring is required")
	case o.UIJobStreams == nil:
		return nil, errors.New("api listener: UIJobStreams is required")
	case o.Config.API.EnableAcme && o.Acme == nil:
		return nil, errors.New("api listener: Acme is required with enable_acme")
	}
	uiStreams := o.UIJobStreams
	fingerprint := o.Fingerprint

	config := o.Config

	if config.Server.CheckPortTimeout > DefaultMaxCheckPortTimeout {
		return nil, fmt.Errorf("'check_port_timeout' can not be more than %s", DefaultMaxCheckPortTimeout)
	}

	vaultLogger := logger.NewLogger("vault", config.Logging.LogOutput, config.Logging.LogLevel)

	repos := o.Repositories

	notificationsLogger := logger.NewLogger("server", config.Logging.LogOutput, config.Logging.LogLevel).Fork("notifications")

	store := repos.Notifications
	scriptConsumer := scriptRunner.NewConsumer(notificationsLogger.Fork("scriptrunner"), config.Notifications.NotificationScriptDir)

	notificationConsumers := []notifications.Consumer{scriptConsumer}

	smtpConfig, err := rmailer.ConfigFromSMTPConfig(config.SMTP)
	if err == nil {
		smtpLogger := notificationsLogger.Fork("smtp")
		smtpLogger.Debugf("using smtp config: %v", smtpConfig)
		mailConsumer := rmailer.NewConsumer(rmailer.NewRMailer(smtpConfig, smtpLogger), smtpLogger)
		notificationConsumers = append(notificationConsumers, mailConsumer)
	} else {
		notificationsLogger.Errorf("failed to bootstrap smtp notifications: %v", err)
		logConsumer := toLog.NewLogConsumer(notificationsLogger.Fork("smtp undeliverable"), notifications.TargetMail) // consume mail notifications even if mailer is not available
		notificationConsumers = append(notificationConsumers, logConsumer)
	}

	notificationProcessor := notifications.NewProcessor(notificationsLogger, store, notificationConsumers...)
	var notificationsCleaner notificationsSQLite.Closeable
	if cleanable, ok := store.(notificationsSQLite.Cleanable); ok && !o.NoNotificationsCleaner {
		notificationLogger := logger.NewLogger("cleaner", config.Logging.LogOutput, logger.LogLevelInfo)
		notificationsCleaner = notificationsSQLite.StartCleaner(ctx, notificationLogger, cleanable, config.Notifications.LogStorageDuration, config.Notifications.CleanupInterval)
	} else if !ok {
		notificationsLogger.Debugf("the notifications repository does not delete old entries; the cleaner is not started")
	}

	tokenManager := authorization.NewManager(repos.APITokens)

	userService := users.NewAPIServiceFromConfig(ctx, repos.Users, config)

	var HTTPServerOptions []chshare.ServerOption
	if config.API.CertFile != "" && config.API.KeyFile != "" {
		HTTPServerOptions = []chshare.ServerOption{chshare.WithTLS(config.API.CertFile, config.API.KeyFile, security.TLSConfig(config.API.TLSMin))}
	}
	// no need for TLS on the api listener when using caddy for API access
	if config.CaddyEnabled() && config.Caddy.APIReverseProxyEnabled() {
		HTTPServerOptions = nil
	}

	if config.API.EnableAcme {
		o.Acme.AddHost(config.API.BaseURL)
		tlsConfig := o.Acme.ApplyTLSConfig(security.TLSConfig(config.API.TLSMin))
		HTTPServerOptions = []chshare.ServerOption{
			chshare.WithTLS("", "", tlsConfig),
		}
	}

	allog := logger.NewLogger("api-listener", config.Logging.LogOutput, config.Logging.LogLevel)
	a := &APIListener{
		apiServices: &apiServices{
			config:               config,
			clientService:        o.ClientService,
			clientAuthProvider:   repos.ClientAuth,
			jobProvider:          repos.Jobs,
			clientGroupProvider:  repos.ClientGroups,
			monitoringService:    o.Monitoring,
			dispatcher:           o.Dispatcher,
			jobRunner:            o.JobRunner,
			libraryManager:       o.Library,
			scheduleManager:      o.Schedules,
			auditLog:             o.AuditLog,
			filesAPI:             o.Files,
			plusManager:          o.PlusManager,
			uiJobWebSockets:      uiStreams,
			capabilitiesOverride: o.Capabilities,
		},
		Logger:                 allog,
		fingerprint:            fingerprint,
		httpServer:             chshare.NewHTTPServer(int(config.API.MaxRequestBytes), allog, HTTPServerOptions...),
		requestLogOptions:      config.InitRequestLogOptions(),
		bannedUsers:            security.NewBanList(time.Duration(config.API.UserLoginWait) * time.Second),
		userService:            userService,
		vaultManager:           vault.NewManagerWithKeyProvider(repos.Vault, &vault.Aes256PassManager{}, o.VaultKeys, vaultLogger),
		tokenManager:           tokenManager,
		storedTunnels:          storedtunnels.New(repos.StoredTunnels),
		notificationsStorage:   store,
		notificationsProcessor: notificationProcessor,
		notificationsCleaner:   notificationsCleaner,
		tenantResolver:         o.TenantResolver,
		usage:                  o.Usage,
		meter:                  o.Meter,
		billing:                o.Billing,
	}

	a.errResponseLogger = allog.Fork("error-response")
	if o.Operator != nil {
		var ob billing.OperatorService
		if o.Billing != nil {
			ob = o.Billing.Operator
		}
		a.operatorAPI = operator.NewHandler(operator.Options{Service: o.Operator, Usage: o.Usage, Billing: ob, CurrentUser: a.getUserModelForAuth, Respond: a.responder()})
	}

	logGroupPermissionsNotice(userService, a.Logger)

	if config.Pairing.Enabled {
		a.initPairing()
		a.Logger.Infof("built-in pairing service enabled, codes are valid for %s", config.Pairing.CodeTTL)
	}

	if config.API.IsTwoFAOn() {
		var msgSrv message.Service
		switch config.API.TwoFATokenDelivery {
		case "pushover":
			msgSrv = message.NewPushoverService(config.Pushover.APIToken)
		case "smtp":
			msgSrv, err = message.NewSMTPService(
				config.SMTP.Server,
				config.SMTP.AuthUsername,
				config.SMTP.AuthPassword,
				config.SMTP.SenderEmail,
				config.SMTP.Secure,
			)
			if err != nil {
				return nil, fmt.Errorf("failed to init smtp service: %v", err)
			}
		default:
			if _, err := exec.LookPath(config.API.TwoFATokenDelivery); err == nil {
				msgSrv = message.NewScriptService(
					config.API.TwoFATokenDelivery,
					config.API.TwoFASendToType,
					config.API.TwoFASendToRegexCompiled,
				)
			} else {
				msgSrv = message.NewURLService(config.API.TwoFATokenDelivery, config.API.BaseURL)
			}
		}

		a.twoFASrv = NewTwoFAService(
			config.API.TwoFATokenTTLSeconds,
			config.API.TwoFASendTimeout,
			userService,
			msgSrv,
		)
		userService.DeliverySrv = msgSrv
		a.Logger.Infof("2FA is enabled via using %s", config.API.TwoFATokenDelivery)
	}

	if config.API.TotPEnabled {
		a.twoFASrv = NewTwoFAService(
			config.API.TwoFATokenTTLSeconds,
			config.API.TwoFASendTimeout,
			userService,
			nil,
		)
		a.Logger.Infof("2FA is enabled via an Authenticator app")
	}

	if config.API.MaxFailedLogin > 0 && config.API.BanTime > 0 {
		a.bannedIPs = security.NewMaxBadAttemptsBanList(
			config.API.MaxFailedLogin,
			time.Duration(config.API.BanTime)*time.Second,
			a.Logger,
		)
	}

	if config.API.AccessLogFile != "" {
		accessLogFile, err := os.OpenFile(config.API.AccessLogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, err
		}
		a.accessLogFile = accessLogFile
	}

	a.apiSessions, err = session.NewCache(ctx, bearer.DefaultTokenLifetime, cleanupAPISessionsInterval, repos.APISessions, nil)
	if err != nil {
		return nil, err
	}

	a.initRouter()

	return a, nil
}

func (al *APIListener) Start(ctx context.Context, addr string) error {
	al.Infof("API Listening on %s...", addr)

	err := al.httpServer.GoListenAndServe(ctx, addr, al.router)
	if err != nil {
		return err
	}

	return nil
}

func (al *APIListener) Wait() error {
	if al.httpServer == nil {
		return nil
	}
	return al.httpServer.Wait()
}

func (al *APIListener) Close() error {
	al.Log().Debugf("closing APIListener...")

	g := &errgroup.Group{}
	if al.httpServer != nil {
		g.Go(al.httpServer.Close)
	}
	if al.accessLogFile != nil {
		g.Go(al.accessLogFile.Close)
	}

	if al.vaultManager != nil {
		g.Go(al.vaultManager.Close)
	}

	if al.apiSessions != nil {
		g.Go(al.apiSessions.Close)
	}

	if al.uploads != nil {
		g.Go(al.uploads.Close)
	}

	g.Go(al.notificationsStorage.Close)
	g.Go(al.notificationsProcessor.Close)

	err := g.Wait()

	al.Log().Debugf("APIListener closed")
	return err
}

var ErrTooManyRequests = errors.New("too many requests, please try later")
var ErrThatPasswordHasExpired = errors.New("password has expired, please change your password")
var ErrCantLoadThatToken = errors.New("there was a problem accessing that token with the provided prefix")
var ErrPrefixNotFound = errors.New("there is no token with that prefix")
var ErrInvalidScopeOfThatToken = errors.New("the scope of the provided token is not authorized for this operation")
var ErrThatTokenHasExpired = errors.New("the provided token has expired")

// lookupUser is used to get the user on every request in auth middleware
func (al *APIListener) lookupUser(r *http.Request, isBearerOnly bool) (authorized bool, username string, err error) {
	if !isBearerOnly {
		if basicUser, basicPwd, basicAuthProvided := r.BasicAuth(); basicAuthProvided {
			return al.handleBasicAuth(r.Context(), r.Method, r.URL.Path, basicUser, basicPwd)
		}
	}

	if bearerToken, bearerAuthProvided := bearer.GetBearerToken(r); bearerAuthProvided {
		isAuthorized, token, err := al.checkBearerToken(r.Context(), bearerToken, r.URL.Path, r.Method)
		if err != nil {
			return isAuthorized, "", err
		}
		if token == nil {
			return false, "", nil
		}

		return isAuthorized, token.AppClaims.Username, nil
	}

	// case when no auth method is provided
	if al.isUserBanned(r.Context(), "") {
		return false, "", ErrTooManyRequests
	}

	return false, "", nil
}

// handleBasicAuth checks username and password against either user's password or token
func (al *APIListener) handleBasicAuth(ctx context.Context, httpverb, urlpath, username, password string) (authorized bool, name string, err error) {
	if al.isUserBanned(ctx, username) {
		return false, username, ErrTooManyRequests
	}

	if username == "" {
		return false, "", nil
	}

	user, err := al.userService.GetByUsername(ctx, username)
	if err != nil {
		return false, username, fmt.Errorf("failed to get user: %w", err)
	}
	if user == nil {
		return false, username, nil
	}

	if user.PasswordExpired != nil && *user.PasswordExpired {
		return false, username, ErrThatPasswordHasExpired
	}

	// skip basic auth with password when 2fa is enabled
	if !al.config.API.IsTwoFAOn() && !al.config.API.TotPEnabled {
		passwordOk := verifyPassword(user.Password, password)
		if passwordOk {
			return true, username, nil
		}
	}

	// only check token if we have one saved == I can't know if I have the token for this operation until I check the prefix inside the db
	//   only check token if the prefix gives notificationsSQLite a match with the db
	// TODO: this type of tokens "User tokens", meant to be used by scripts - used in place of the password at each request - should be renamed "passwords" or "long lived passwords" or "encrypted long lived passwords"
	prefix, password, err := authorization.Extract(password)
	if err != nil {
		return false, username, nil
	}
	userToken, err := al.tokenManager.Get(ctx, username, prefix)
	if err != nil {
		return false, username, err
	}

	if userToken != nil {
		if userToken.ExpiresAt != nil {
			if userToken.ExpiresAt.Before(time.Now()) {
				return false, username, nil
			}
		}
		tokenOk := verifyPassword(userToken.Token, password)
		if tokenOk {
			switch userToken.Scope {
			case authorization.APITokenRead:
				if httpverb == "GET" && !strings.Contains(urlpath, "/ws") {
					return true, username, nil
				}
			case authorization.APITokenReadWrite:
				return true, username, nil
			case authorization.APITokenClientsAuth:
				if isClientsAuthRoute(urlpath) {
					return true, username, nil
				}
			}
			return false, username, ErrInvalidScopeOfThatToken
		}
	}

	return false, username, nil
}

// isClientsAuthRoute reports whether urlpath is one of the client credential routes a clients-auth token may use:
// the path, not any path containing the text (a client ID, a user name or an organization id may contain
// "clients-auth").
func isClientsAuthRoute(urlpath string) bool {
	rest := strings.TrimPrefix(urlpath, routes.AllRoutesPrefix)
	return rest == "/clients-auth" || strings.HasPrefix(rest, "/clients-auth/")
}

func (al *APIListener) checkBearerToken(ctx context.Context, bearerToken, uri, method string) (bool, *bearer.TokenContext, error) {
	tokenCtx, err := bearer.ParseToken(bearerToken, al.config.API.JWTSecret)
	if err != nil {
		// A malformed, wrongly signed or expired token means "not authenticated": respond
		// 401 (so clients send the user to the login page), not 500.
		al.Debugf("failed to parse jwt token: %v", err)
		return false, nil, nil
	}

	if al.isUserBanned(ctx, tokenCtx.AppClaims.Username) {
		al.Errorf(
			"User %s is banned",
			tokenCtx.AppClaims.Username,
		)
		return false, nil, ErrTooManyRequests
	}

	authorized, apiSession, err := bearer.ValidateBearerToken(
		ctx,
		tokenCtx,
		uri,
		method,
		al.apiSessions,
		al.Logger)
	if err != nil {
		return false, tokenCtx, err
	}
	if authorized {
		// extend the token lifetime by a short amount so that in-progress activities can complete
		if err := bearer.IncreaseSessionLifetime(ctx, al.apiSessions, apiSession); err != nil {
			// do not return error since it should respond with 401 instead of 500, just log it
			al.Errorf("Failed to increase jwt token lifetime: %v", err)
		}
	}
	return authorized, tokenCtx, nil
}

const htpasswdBcryptPrefix = "$2y$"

// validateCredentials returns true if given credentials belong to a user with access to the API.
func (al *APIListener) validateCredentials(ctx context.Context, username, password string, skipPasswordValidation bool) (bool, *users.User, error) {
	if username == "" {
		return false, nil, nil
	}

	user, err := al.userService.GetByUsername(ctx, username)
	if err != nil {
		return false, nil, fmt.Errorf("failed to get user: %w", err)
	}

	if al.shouldCreateMissingUser(user, skipPasswordValidation) {
		pswd, err := random.UUID4()
		if err != nil {
			return false, nil, err
		}
		user = &users.User{
			Username: username,
			Password: pswd,
		}
		if al.config.API.DefaultUserGroup != "" {
			user.Groups = []string{al.config.API.DefaultUserGroup}
		}
		err = al.userService.Change(ctx, user, "")
		if err != nil {
			return false, user, fmt.Errorf("failed to create missing user: %v", err)
		}
	}

	if user == nil {
		return false, user, nil
	}

	if skipPasswordValidation {
		return true, user, nil
	}

	return verifyPassword(user.Password, password), user, nil
}

func (al *APIListener) shouldCreateMissingUser(user *users.User, skipPasswordValidation bool) bool {
	if user != nil || !skipPasswordValidation {
		return false
	}
	if al.config.API.CreateMissingUsers {
		return true
	}
	// Plus OAuth without a permitted-user list creates users. Without Plus OAuth enabled this must
	// not apply: IsOAuthPermittedUserList is false then, which used to let header auth create admins.
	return rportplus.IsPlusOAuthEnabled(al.config.PlusConfig) && !rportplus.IsOAuthPermittedUserList(al.config.PlusConfig)
}

func verifyPassword(saved, provided string) bool {
	// bcrypt hashed password
	if strings.HasPrefix(saved, htpasswdBcryptPrefix) {
		return bcrypt.CompareHashAndPassword([]byte(saved), []byte(provided)) == nil
	}

	// plaintext password, constant time compare is used for security reasons
	return subtle.ConstantTimeCompare([]byte(saved), []byte(provided)) == 1
}

const WebSocketAccessTokenQueryParam = "access_token"

var (
	errUnauthorized        = errors.New("unauthorized")
	errAccessTokenRequired = errors.New("token required")
)

func (al *APIListener) wsAuth(f http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var authorized bool
		var username string
		var err error

		tokenStr := r.URL.Query().Get(WebSocketAccessTokenQueryParam)
		if tokenStr == "" {
			basicUser, basicPwd, basicAuthProvided := r.BasicAuth()

			if basicAuthProvided {
				authorized, username, err = al.handleBasicAuth(r.Context(), r.Method, r.URL.Path, basicUser, basicPwd)
			} else {
				if !al.handleBannedIPs(r, false) {
					return
				}
				al.jsonErrorResponse(w, http.StatusUnauthorized, errAccessTokenRequired)
				return
			}
		} else {
			var token *bearer.TokenContext
			authorized, token, err = al.checkBearerToken(r.Context(), tokenStr, r.URL.Path, r.Method)
			if authorized && err == nil {
				username = token.AppClaims.Username
			}
		}

		if err != nil {
			if errors.Is(err, ErrTooManyRequests) {
				al.jsonErrorResponse(w, http.StatusTooManyRequests, err)
				return
			}
			al.jsonErrorResponse(w, http.StatusInternalServerError, err)
			return
		}

		if !al.handleBannedIPs(r, authorized) {
			return
		}

		if !authorized || username == "" {
			al.banUser(r.Context(), username)
			al.jsonErrorResponse(w, http.StatusUnauthorized, errUnauthorized)
			return
		}

		newCtx := api.WithUser(r.Context(), username)
		f.ServeHTTP(w, r.WithContext(newCtx))
	}
}

// logGroupPermissionsNotice tells admins when permission-protected features are limited to Administrators.
func logGroupPermissionsNotice(userService *users.APIService, log *logger.Logger) {
	if userService == nil || userService.Provider == nil {
		return // no API users configured (API disabled)
	}
	if !userService.SupportsGroupPermissions() {
		log.Infof("the configured user store has no group permissions (auth_group_details_table): " +
			"only Administrators can use commands, scripts, tunnels, vault and other permission-protected features")
	}
}
