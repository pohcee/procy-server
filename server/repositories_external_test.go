package chserver_test

// This file is an external test package: it can only use exported names. Every stub below implements its interface
// by writing out each method (no embedded interface), which proves that a package outside this module can implement
// and inject every repository of chserver.Repositories.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	chserver "github.com/pohcee/procy-server/server"
	"github.com/pohcee/procy-server/server/api/authorization"
	"github.com/pohcee/procy-server/server/api/jobs"
	"github.com/pohcee/procy-server/server/api/jobs/schedule"
	"github.com/pohcee/procy-server/server/api/session"
	"github.com/pohcee/procy-server/server/api/users"
	"github.com/pohcee/procy-server/server/auditlog"
	"github.com/pohcee/procy-server/server/cgroups"
	"github.com/pohcee/procy-server/server/chconfig"
	"github.com/pohcee/procy-server/server/clients"
	"github.com/pohcee/procy-server/server/clients/clientdata"
	"github.com/pohcee/procy-server/server/clients/storedtunnels"
	"github.com/pohcee/procy-server/server/clientsauth"
	"github.com/pohcee/procy-server/server/library"
	"github.com/pohcee/procy-server/server/monitoring"
	"github.com/pohcee/procy-server/server/notifications"
	"github.com/pohcee/procy-server/server/vault"
	"github.com/pohcee/procy-server/share/enums"
	"github.com/pohcee/procy-server/share/files"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/models"
	"github.com/pohcee/procy-server/share/query"
)

type extClients struct{}

func (extClients) GetAll(context.Context, *logger.Logger) ([]*clientdata.Client, error) {
	return nil, nil
}
func (extClients) Save(context.Context, *clientdata.Client) error       { return nil }
func (extClients) DeleteObsolete(context.Context, *logger.Logger) error { return nil }
func (extClients) Delete(context.Context, string, *logger.Logger) error { return nil }
func (extClients) Close() error                                         { return nil }

type extStoredTunnels struct{}

func (extStoredTunnels) Delete(context.Context, string, string) error              { return nil }
func (extStoredTunnels) Insert(context.Context, *storedtunnels.StoredTunnel) error { return nil }
func (extStoredTunnels) Update(context.Context, *storedtunnels.StoredTunnel) error { return nil }
func (extStoredTunnels) List(context.Context, string, *query.ListOptions) ([]*storedtunnels.StoredTunnel, error) {
	return nil, nil
}
func (extStoredTunnels) Count(context.Context, string, *query.ListOptions) (int, error) {
	return 0, nil
}

type extClientGroups struct{}

func (extClientGroups) Get(context.Context, string) (*cgroups.ClientGroup, error) { return nil, nil }
func (extClientGroups) GetAll(context.Context) ([]*cgroups.ClientGroup, error)    { return nil, nil }
func (extClientGroups) List(context.Context, *query.ListOptions) ([]*cgroups.ClientGroup, error) {
	return nil, nil
}
func (extClientGroups) Create(context.Context, *cgroups.ClientGroup) error { return nil }
func (extClientGroups) Update(context.Context, *cgroups.ClientGroup) error { return nil }
func (extClientGroups) Delete(context.Context, string) error               { return nil }
func (extClientGroups) Close() error                                       { return nil }

type extJobs struct{}

func (extJobs) GetByJID(context.Context, string, string) (*models.Job, error)   { return nil, nil }
func (extJobs) List(context.Context, *query.ListOptions) ([]*models.Job, error) { return nil, nil }
func (extJobs) Count(context.Context, *query.ListOptions) (int, error)          { return 0, nil }
func (extJobs) SaveJob(context.Context, *models.Job) error                      { return nil }
func (extJobs) CreateJob(context.Context, *models.Job) error                    { return nil }
func (extJobs) GetMultiJob(context.Context, string) (*models.MultiJob, error)   { return nil, nil }
func (extJobs) CountMultiJobs(context.Context, *query.ListOptions) (int, error) { return 0, nil }
func (extJobs) SaveMultiJob(context.Context, *models.MultiJob) error            { return nil }
func (extJobs) CleanupJobsMultiJobs(context.Context, int) error                 { return nil }
func (extJobs) Close() error                                                    { return nil }
func (extJobs) GetMultiJobSummaries(context.Context, *query.ListOptions) ([]*models.MultiJobSummary, error) {
	return nil, nil
}

type extSchedules struct{}

func (extSchedules) Insert(context.Context, *schedule.Schedule) error { return nil }
func (extSchedules) Update(context.Context, *schedule.Schedule) error { return nil }
func (extSchedules) List(context.Context, *query.ListOptions) ([]*schedule.Schedule, error) {
	return nil, nil
}
func (extSchedules) Get(context.Context, string) (*schedule.Schedule, error)       { return nil, nil }
func (extSchedules) Delete(context.Context, string) error                          { return nil }
func (extSchedules) CountJobsInProgress(context.Context, string, int) (int, error) { return 0, nil }

type extMonitoring struct{}

func (extMonitoring) CreateMeasurement(context.Context, *models.Measurement) error { return nil }
func (extMonitoring) DeleteMeasurementsBefore(context.Context, time.Time) (int64, error) {
	return 0, nil
}
func (extMonitoring) ListGraphByClientID(context.Context, string, float64, *query.ListOptions, string) ([]*monitoring.ClientGraphMetricsGraphPayload, error) {
	return nil, nil
}
func (extMonitoring) ListGraphMetricsByClientID(context.Context, string, float64, *query.ListOptions) ([]*monitoring.ClientGraphMetricsPayload, error) {
	return nil, nil
}
func (extMonitoring) ListMetricsByClientID(context.Context, string, *query.ListOptions) ([]*monitoring.ClientMetricsPayload, error) {
	return nil, nil
}
func (extMonitoring) ListMountpointsByClientID(context.Context, string, *query.ListOptions) ([]*monitoring.ClientMountpointsPayload, error) {
	return nil, nil
}
func (extMonitoring) ListProcessesByClientID(context.Context, string, *query.ListOptions) ([]*monitoring.ClientProcessesPayload, error) {
	return nil, nil
}
func (extMonitoring) CountByClientID(context.Context, string, *query.ListOptions) (int, error) {
	return 0, nil
}
func (extMonitoring) Close() error { return nil }

type extNotifications struct{}

func (extNotifications) List(context.Context, *query.ListOptions) ([]notifications.NotificationSummary, error) {
	return nil, nil
}
func (extNotifications) Count(context.Context, *query.ListOptions) (int, error) { return 0, nil }
func (extNotifications) Details(context.Context, string) (notifications.NotificationDetails, bool, error) {
	return notifications.NotificationDetails{}, false, nil
}
func (extNotifications) Create(context.Context, notifications.NotificationDetails) error { return nil }
func (extNotifications) SetDone(context.Context, notifications.NotificationDetails, string) error {
	return nil
}
func (extNotifications) SetError(context.Context, notifications.NotificationDetails, string, string) error {
	return nil
}
func (extNotifications) NotificationStream(notifications.Target) chan notifications.NotificationDetails {
	return nil
}
func (extNotifications) Close() error { return nil }

type extLibrary struct{}

func (extLibrary) Insert(context.Context, library.Entry, library.Version) error          { return nil }
func (extLibrary) AddVersion(context.Context, library.Entry, library.Version, int) error { return nil }
func (extLibrary) Get(context.Context, string) (*library.Entry, error)                   { return nil, nil }
func (extLibrary) List(context.Context, bool) ([]library.Entry, error)                   { return nil, nil }
func (extLibrary) ListLive(context.Context) ([]library.Entry, error)                     { return nil, nil }
func (extLibrary) Versions(context.Context, string) ([]library.Version, error)           { return nil, nil }
func (extLibrary) Version(context.Context, string, int) (*library.Version, error)        { return nil, nil }
func (extLibrary) SetDeleted(context.Context, string, *time.Time, string) error          { return nil }
func (extLibrary) Prune(context.Context, int, time.Time) (int64, error)                  { return 0, nil }
func (extLibrary) Meta(context.Context, string) (string, bool, error)                    { return "", false, nil }
func (extLibrary) ByBuiltinKey(context.Context, string) (*library.Entry, error)          { return nil, nil }
func (extLibrary) Close() error                                                          { return nil }

type extAPITokens struct{}

func (extAPITokens) Get(context.Context, string, string) (*authorization.APIToken, error) {
	return nil, nil
}
func (extAPITokens) GetByName(context.Context, string, string) (*authorization.APIToken, error) {
	return nil, nil
}
func (extAPITokens) GetAll(context.Context, string) ([]*authorization.APIToken, error) {
	return nil, nil
}
func (extAPITokens) Save(context.Context, *authorization.APIToken) error { return nil }
func (extAPITokens) Delete(context.Context, string, string) error        { return nil }
func (extAPITokens) Close() error                                        { return nil }

type extAPISessions struct{}

func (extAPISessions) Get(context.Context, int64) (bool, session.APISession, error) {
	return false, session.APISession{}, nil
}
func (extAPISessions) GetAll(context.Context) ([]session.APISession, error)    { return nil, nil }
func (extAPISessions) Save(context.Context, session.APISession) (int64, error) { return 0, nil }
func (extAPISessions) Delete(context.Context, int64) error                     { return nil }
func (extAPISessions) DeleteExpired(context.Context) error                     { return nil }
func (extAPISessions) Close() error                                            { return nil }
func (extAPISessions) DeleteAllByUser(context.Context, string) error           { return nil }
func (extAPISessions) DeleteByID(context.Context, string, int64) error         { return nil }

type extVault struct{}

func (extVault) GetStatus(context.Context) (vault.DbStatus, error) { return vault.DbStatus{}, nil }
func (extVault) SetStatus(context.Context, vault.DbStatus) error   { return nil }
func (extVault) GetByID(context.Context, int) (vault.StoredValue, bool, error) {
	return vault.StoredValue{}, false, nil
}
func (extVault) List(context.Context, *query.ListOptions) ([]vault.ValueKey, error) { return nil, nil }
func (extVault) FindByKeyAndClientID(context.Context, string, string) (vault.StoredValue, bool, error) {
	return vault.StoredValue{}, false, nil
}
func (extVault) Save(context.Context, string, int64, *vault.InputValue, time.Time) (int64, error) {
	return 0, nil
}
func (extVault) Delete(context.Context, int) error { return nil }
func (extVault) Close() error                      { return nil }

type extVaultFactory struct{}

func (extVaultFactory) GetDbProvider() vault.DbProvider { return extVault{} }
func (extVaultFactory) Init() error                     { return nil }

type extAuditLog struct{}

func (extAuditLog) Close() error                                { return nil }
func (extAuditLog) Save(context.Context, *auditlog.Entry) error { return nil }
func (extAuditLog) List(context.Context, *query.ListOptions) ([]*auditlog.Entry, error) {
	return nil, nil
}
func (extAuditLog) Count(context.Context, *query.ListOptions) (int, error) { return 0, nil }

type extUsers struct{}

func (extUsers) Type() enums.ProviderSource                                 { return enums.ProviderSourceStatic }
func (extUsers) SupportsGroupPermissions() bool                             { return false }
func (extUsers) GetAll(context.Context) ([]*users.User, error)              { return nil, nil }
func (extUsers) ListGroups(context.Context) ([]users.Group, error)          { return nil, nil }
func (extUsers) GetGroup(context.Context, string) (users.Group, error)      { return users.Group{}, nil }
func (extUsers) UpdateGroup(context.Context, string, users.Group) error     { return nil }
func (extUsers) DeleteGroup(context.Context, string) error                  { return nil }
func (extUsers) GetByUsername(context.Context, string) (*users.User, error) { return nil, nil }
func (extUsers) Add(context.Context, *users.User) error                     { return nil }
func (extUsers) Update(context.Context, *users.User, string) error          { return nil }
func (extUsers) Delete(context.Context, string) error                       { return nil }

type extClientAuth struct{}

func (extClientAuth) Get(context.Context, string) (*clientsauth.ClientAuth, error) { return nil, nil }
func (extClientAuth) GetFiltered(context.Context, *query.ListOptions) ([]*clientsauth.ClientAuth, int, error) {
	return nil, 0, nil
}
func (extClientAuth) Add(context.Context, *clientsauth.ClientAuth) (bool, error) { return false, nil }
func (extClientAuth) Delete(context.Context, string) error                       { return nil }
func (extClientAuth) IsWriteable() bool                                          { return false }
func (extClientAuth) Source() enums.ProviderSource                               { return enums.ProviderSourceStatic }

// Compile-time proof that every stub satisfies its interface.
var (
	_ clients.ClientStore         = extClients{}
	_ storedtunnels.Provider      = extStoredTunnels{}
	_ cgroups.ClientGroupProvider = extClientGroups{}
	_ jobs.Provider               = extJobs{}
	_ schedule.Provider           = extSchedules{}
	_ monitoring.DBProvider       = extMonitoring{}
	_ notifications.Repository    = extNotifications{}
	_ library.Repository          = extLibrary{}
	_ authorization.DbProvider    = extAPITokens{}
	_ session.StorageProvider     = extAPISessions{}
	_ vault.DbProvider            = extVault{}
	_ vault.DbProviderFactory     = extVaultFactory{}
	_ auditlog.Provider           = extAuditLog{}
	_ users.Provider              = extUsers{}
	_ clientsauth.Provider        = extClientAuth{}
)

func TestRepositoriesImplementableOutsideThePackage(t *testing.T) {
	r := chserver.Repositories{
		Clients:       extClients{},
		StoredTunnels: extStoredTunnels{},
		ClientGroups:  extClientGroups{},
		Jobs:          extJobs{},
		Schedules:     extSchedules{},
		Monitoring:    extMonitoring{},
		Notifications: extNotifications{},
		Library:       extLibrary{},
		APITokens:     extAPITokens{},
		APISessions:   extAPISessions{},
		Vault:         extVaultFactory{},
		AuditLog:      extAuditLog{},
		Users:         extUsers{},
		ClientAuth:    extClientAuth{},
	}
	opts := chserver.ServerOpts{Repositories: &r, SkipMigrations: true}
	if opts.Repositories.Clients == nil || !opts.SkipMigrations {
		t.Fatal("repositories not assigned")
	}
}

// TestNewServerWithPlainRepositories starts and closes a server on a temp data directory with a notification log
// that has no cleaner (no sqlite.Cleanable) and a library without the legacy import (no library.LegacyImporter).
func TestNewServerWithPlainRepositories(t *testing.T) {
	config := &chconfig.Config{
		Server: chconfig.ServerConfig{
			DataDir:      t.TempDir(),
			Auth:         "client:secret",
			AuthID:       "client",
			AuthPassword: "secret",
		},
		API: chconfig.APIConfig{Auth: "admin:foobaz"},
		Logging: chconfig.LogConfig{
			LogOutput: logger.LogOutput{File: os.Stdout},
			LogLevel:  logger.LogLevelInfo,
		},
	}
	opts := &chserver.ServerOpts{
		FilesAPI:     files.NewFileSystem(),
		Repositories: &chserver.Repositories{Notifications: extNotifications{}, Library: extLibrary{}},
	}

	s, err := chserver.NewServer(context.Background(), config, opts)
	require.NoError(t, err)

	assert.NoError(t, s.Close())
}
