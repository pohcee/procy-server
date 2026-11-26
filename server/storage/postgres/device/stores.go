package device

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/server/api/jobs"
	"github.com/pohcee/procy-server/server/api/users"
	"github.com/pohcee/procy-server/server/auditlog"
	"github.com/pohcee/procy-server/server/cgroups"
	"github.com/pohcee/procy-server/server/clients"
	"github.com/pohcee/procy-server/server/clients/clientdata"
	"github.com/pohcee/procy-server/server/clients/storedtunnels"
	"github.com/pohcee/procy-server/server/clientsauth"
	"github.com/pohcee/procy-server/server/monitoring"
	"github.com/pohcee/procy-server/server/storage/outbox"
	"github.com/pohcee/procy-server/server/storage/postgres"
	pgclientauth "github.com/pohcee/procy-server/server/storage/postgres/clientauth"
	pgjobs "github.com/pohcee/procy-server/server/storage/postgres/jobs"
	"github.com/pohcee/procy-server/share/enums"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/models"
	"github.com/pohcee/procy-server/share/query"
	"github.com/pohcee/procy-server/share/tenant"
)

// ErrNotOnDeviceProcess is returned for what only api processes do (writing admin-set data, listing history).
var ErrNotOnDeviceProcess = errors.New("not available on a device process: use the API")

// newKey returns a random idempotency key.
func newKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// enqueue adds item (the fields of its kind) to ob with the tenant of ctx and key.
func enqueue(ctx context.Context, ob *outbox.Outbox, kind outbox.Kind, key string, item map[string]any) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	for k, v := range item {
		// a NUL can't be stored in Postgres text or jsonb: one would make the whole batch fail
		if str, ok := v.(string); ok && strings.ContainsRune(str, 0) {
			item[k] = strings.ReplaceAll(str, "\x00", "")
		}
	}
	item["tenant_id"] = string(tid)
	item["key"] = key
	b, err := json.Marshal(item)
	if err != nil {
		return err
	}
	return ob.Add(ctx, kind, tid, key, b)
}

// ---------------------------------------------------------------------------------------------------------------

// ClientAuth checks client credentials through device_client_auth (hashes only). It is read-only.
type ClientAuth struct{ DB *sqlx.DB }

var (
	_ clientsauth.Provider = ClientAuth{}
	_ clientsauth.Verifier = ClientAuth{}
)

// Lookup returns the tenant and stored hash of the credentials id, found across tenants, or found = false.
func (a ClientAuth) Lookup(ctx context.Context, id string) (tid tenant.ID, hash string, found bool, err error) {
	var rows []struct {
		TenantID string `db:"tenant_id"`
		ID       string `db:"id"`
		Hash     string `db:"password_hash"`
	}
	if err := a.DB.SelectContext(ctx, &rows, "SELECT tenant_id, id, password_hash FROM device_client_auth($1)", id); err != nil {
		return "", "", false, fmt.Errorf("client auth: lookup %q: %w", id, err)
	}
	if len(rows) == 0 {
		return "", "", false, nil
	}
	return tenant.ID(rows[0].TenantID), rows[0].Hash, true, nil
}

// Get returns the credentials id (without the password) when they belong to the context's tenant.
func (a ClientAuth) Get(ctx context.Context, id string) (*clientsauth.ClientAuth, error) {
	want, err := postgres.TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	tid, _, found, err := a.Lookup(ctx, id)
	if err != nil || !found || tid != want {
		return nil, err
	}
	return &clientsauth.ClientAuth{ID: id}, nil
}

// Verify reports whether password matches the stored hash of id in the context's tenant.
func (a ClientAuth) Verify(ctx context.Context, id string, password []byte) (bool, error) {
	want, err := postgres.TenantOf(ctx)
	if err != nil {
		return false, err
	}
	tid, hash, found, err := a.Lookup(ctx, id)
	if err != nil || !found || tid != want {
		return false, err
	}
	return pgclientauth.CheckStored(hash, password), nil
}

func (ClientAuth) GetFiltered(context.Context, *query.ListOptions) ([]*clientsauth.ClientAuth, int, error) {
	return nil, 0, ErrNotOnDeviceProcess
}
func (ClientAuth) Add(context.Context, *clientsauth.ClientAuth) (bool, error) {
	return false, ErrNotOnDeviceProcess
}
func (ClientAuth) Delete(context.Context, string) error { return ErrNotOnDeviceProcess }
func (ClientAuth) IsWriteable() bool                    { return false }
func (ClientAuth) Source() enums.ProviderSource         { return enums.ProviderSourceDB }

// ---------------------------------------------------------------------------------------------------------------

// Clients is the client store of a device process: a client is looked up with device_client; what a connected
// client reports is saved as client state (report_client_state) and its connections and disconnections as
// connection events, both through the outbox. Clients are not loaded at start (GetAll returns none: the device role
// has no snapshot access); a client is known to this process from its connection on. Deleting clients is left to
// the api processes.
type Clients struct {
	DB     *sqlx.DB
	Outbox *outbox.Outbox
	NodeID string

	mu        sync.Mutex
	connected map[string]bool // tenant/client id → last saved connection state
}

var _ clients.ClientStore = (*Clients)(nil)

func (c *Clients) GetAll(context.Context, *logger.Logger) ([]*clientdata.Client, error) {
	return []*clientdata.Client{}, nil
}

// Get returns the client id of the context's tenant as the API sees it, or nil.
func (c *Clients) Get(ctx context.Context, id string, l *logger.Logger) (*clientdata.Client, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID             string     `db:"id"`
		ClientAuthID   string     `db:"client_auth_id"`
		DisconnectedAt *time.Time `db:"disconnected_at"`
		Details        string     `db:"details"`
	}
	if err := c.DB.SelectContext(ctx, &rows, "SELECT id, client_auth_id, disconnected_at, details FROM device_client($1, $2)", string(tid), id); err != nil {
		return nil, fmt.Errorf("clients: get %q: %w", id, err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	r := rows[0]
	return clients.DecodeStoredClient(clients.StoredClient{ID: r.ID, ClientAuthID: r.ClientAuthID, DisconnectedAt: r.DisconnectedAt, Details: r.Details}, l)
}

// Save reports the client's state, and a connection event when its connection state changed since the last save.
func (c *Clients) Save(ctx context.Context, cl *clientdata.Client) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	sc, err := clients.EncodeStoredClient(cl)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := enqueue(ctx, c.Outbox, outbox.KindClientState, newKey(), map[string]any{
		"client_id": sc.ID, "client_auth_id": sc.ClientAuthID, "disconnected_at": sc.DisconnectedAt,
		"reported_at": now, "details": sc.Details,
	}); err != nil {
		return err
	}
	connected := sc.DisconnectedAt == nil
	k := string(tid) + "/" + sc.ID
	c.mu.Lock()
	if c.connected == nil {
		c.connected = map[string]bool{}
	}
	was, known := c.connected[k]
	if connected {
		c.connected[k] = true
	} else {
		delete(c.connected, k) // a disconnected client is forgotten; its next connection is new
	}
	c.mu.Unlock()
	if known && was == connected || !known && !connected {
		return nil
	}
	event, at := "connected", now
	if !connected {
		event, at = "disconnected", *sc.DisconnectedAt
	}
	return enqueue(ctx, c.Outbox, outbox.KindConnectionEvent, newKey(), map[string]any{
		"client_id": sc.ID, "client_auth_id": sc.ClientAuthID, "event": event, "at": at, "node_id": c.NodeID,
	})
}

func (c *Clients) DeleteObsolete(context.Context, *logger.Logger) error { return nil }
func (c *Clients) Delete(context.Context, string, *logger.Logger) error { return nil }
func (c *Clients) Close() error                                         { return nil }

// ---------------------------------------------------------------------------------------------------------------

// ClientGroups reads the client groups with device_client_groups. Writes are refused.
type ClientGroups struct{ DB *sqlx.DB }

var _ cgroups.ClientGroupProvider = ClientGroups{}

func (g ClientGroups) GetAll(ctx context.Context) ([]*cgroups.ClientGroup, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	res := []*cgroups.ClientGroup{}
	if err := g.DB.SelectContext(ctx, &res, "SELECT id, description, params, allowed_user_groups FROM device_client_groups($1)", string(tid)); err != nil {
		return nil, fmt.Errorf("client groups: %w", err)
	}
	for _, cg := range res {
		cg.TenantID = tid
	}
	return res, nil
}

func (g ClientGroups) Get(ctx context.Context, id string) (*cgroups.ClientGroup, error) {
	all, err := g.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, cg := range all {
		if cg.ID == id {
			return cg, nil
		}
	}
	return nil, nil
}

// List returns all groups; the options (filters, sorts) are for the API, which a device process doesn't serve.
func (g ClientGroups) List(ctx context.Context, _ *query.ListOptions) ([]*cgroups.ClientGroup, error) {
	return g.GetAll(ctx)
}
func (ClientGroups) Create(context.Context, *cgroups.ClientGroup) error { return ErrNotOnDeviceProcess }
func (ClientGroups) Update(context.Context, *cgroups.ClientGroup) error { return ErrNotOnDeviceProcess }
func (ClientGroups) Delete(context.Context, string) error               { return ErrNotOnDeviceProcess }
func (ClientGroups) Close() error                                       { return nil }

// ---------------------------------------------------------------------------------------------------------------

// StoredTunnels reads a client's stored tunnels with device_stored_tunnels. Writes are refused.
type StoredTunnels struct{ DB *sqlx.DB }

var _ storedtunnels.Provider = StoredTunnels{}

// List returns the client's stored tunnels; the options are for the API, which a device process doesn't serve.
func (s StoredTunnels) List(ctx context.Context, clientID string, _ *query.ListOptions) ([]*storedtunnels.StoredTunnel, error) {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return nil, err
	}
	res := []*storedtunnels.StoredTunnel{}
	if err := s.DB.SelectContext(ctx, &res, "SELECT * FROM device_stored_tunnels($1, $2)", string(tid), clientID); err != nil {
		return nil, fmt.Errorf("stored tunnels: %w", err)
	}
	for _, t := range res {
		t.TenantID = tid
	}
	return res, nil
}

func (s StoredTunnels) Count(ctx context.Context, clientID string, o *query.ListOptions) (int, error) {
	l, err := s.List(ctx, clientID, o)
	return len(l), err
}
func (StoredTunnels) Insert(context.Context, *storedtunnels.StoredTunnel) error {
	return ErrNotOnDeviceProcess
}
func (StoredTunnels) Update(context.Context, *storedtunnels.StoredTunnel) error {
	return ErrNotOnDeviceProcess
}
func (StoredTunnels) Delete(context.Context, string, string) error { return ErrNotOnDeviceProcess }

// ---------------------------------------------------------------------------------------------------------------

// Jobs ships the job results clients send through the outbox (ingest_job_results). A device process can't read
// jobs: GetByJID finds none and the lists are empty.
type Jobs struct{ Outbox *outbox.Outbox }

var _ jobs.Provider = Jobs{}

// SaveJob ships the job as it is (created or replaced, as the job store's SaveJob).
func (j Jobs) SaveJob(ctx context.Context, job *models.Job) error { return j.ship(ctx, job, false) }

func (j Jobs) ship(ctx context.Context, job *models.Job, createOnly bool) error {
	cols, err := pgjobs.EncodeJob(job)
	if err != nil {
		return err
	}
	b, err := json.Marshal(cols)
	if err != nil {
		return err
	}
	item := map[string]any{}
	if err := json.Unmarshal(b, &item); err != nil {
		return err
	}
	if createOnly {
		item["create_only"] = true
	}
	return enqueue(ctx, j.Outbox, outbox.KindJobResult, newKey(), item)
}

// CreateJob ships the job to be created if it doesn't exist (as the job store's CreateJob: a result saved first is
// kept, only the job's library fields are added to it).
func (j Jobs) CreateJob(ctx context.Context, job *models.Job) error { return j.ship(ctx, job, true) }

func (Jobs) GetByJID(context.Context, string, string) (*models.Job, error)   { return nil, nil }
func (Jobs) List(context.Context, *query.ListOptions) ([]*models.Job, error) { return nil, nil }
func (Jobs) Count(context.Context, *query.ListOptions) (int, error)          { return 0, nil }
func (Jobs) GetMultiJob(context.Context, string) (*models.MultiJob, error)   { return nil, nil }
func (Jobs) GetMultiJobSummaries(context.Context, *query.ListOptions) ([]*models.MultiJobSummary, error) {
	return nil, nil
}
func (Jobs) CountMultiJobs(context.Context, *query.ListOptions) (int, error) { return 0, nil }
func (Jobs) SaveMultiJob(context.Context, *models.MultiJob) error            { return ErrNotOnDeviceProcess }
func (Jobs) CleanupJobsMultiJobs(context.Context, int) error                 { return nil }
func (Jobs) Close() error                                                    { return nil }

// ---------------------------------------------------------------------------------------------------------------

// Monitoring ships measurements through the outbox (ingest_measurements); queries find nothing.
type Monitoring struct{ Outbox *outbox.Outbox }

var _ monitoring.DBProvider = Monitoring{}

func (m Monitoring) CreateMeasurement(ctx context.Context, ms *models.Measurement) error {
	item := map[string]any{
		"client_id": ms.ClientID, "timestamp": ms.Timestamp, "cpu_usage_percent": ms.CPUUsagePercent,
		"memory_usage_percent": ms.MemoryUsagePercent, "io_usage_percent": ms.IoUsagePercent,
		"processes": ms.Processes, "mountpoints": ms.Mountpoints,
	}
	if ms.NetLan != nil {
		item["net_lan_in"], item["net_lan_out"] = ms.NetLan.In, ms.NetLan.Out
	}
	if ms.NetWan != nil {
		item["net_wan_in"], item["net_wan_out"] = ms.NetWan.In, ms.NetWan.Out
	}
	// one measurement per client and time: the key says so, so a repeat is ignored by the outbox already
	sum := sha256.Sum256([]byte(ms.ClientID + "@" + ms.Timestamp.UTC().Format(time.RFC3339Nano)))
	key := "m-" + hex.EncodeToString(sum[:16])
	return enqueue(ctx, m.Outbox, outbox.KindMeasurement, key, item)
}

func (Monitoring) DeleteMeasurementsBefore(context.Context, time.Time) (int64, error) { return 0, nil }
func (Monitoring) ListGraphByClientID(context.Context, string, float64, *query.ListOptions, string) ([]*monitoring.ClientGraphMetricsGraphPayload, error) {
	return nil, nil
}
func (Monitoring) ListGraphMetricsByClientID(context.Context, string, float64, *query.ListOptions) ([]*monitoring.ClientGraphMetricsPayload, error) {
	return nil, nil
}
func (Monitoring) ListMetricsByClientID(context.Context, string, *query.ListOptions) ([]*monitoring.ClientMetricsPayload, error) {
	return nil, nil
}
func (Monitoring) ListMountpointsByClientID(context.Context, string, *query.ListOptions) ([]*monitoring.ClientMountpointsPayload, error) {
	return nil, nil
}
func (Monitoring) ListProcessesByClientID(context.Context, string, *query.ListOptions) ([]*monitoring.ClientProcessesPayload, error) {
	return nil, nil
}
func (Monitoring) CountByClientID(context.Context, string, *query.ListOptions) (int, error) {
	return 0, nil
}
func (Monitoring) Close() error { return nil }

// ---------------------------------------------------------------------------------------------------------------

// AuditLog ships audit entries through the outbox (ingest_audit); lists are empty.
type AuditLog struct{ Outbox *outbox.Outbox }

var _ auditlog.Provider = AuditLog{}

func (a AuditLog) Save(ctx context.Context, e *auditlog.Entry) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	e.TenantID = tid
	return enqueue(ctx, a.Outbox, outbox.KindAudit, newKey(), map[string]any{
		"timestamp": e.Timestamp, "application": e.Application, "action": e.Action, "username": e.Username,
		"remote_ip": e.RemoteIP, "affected_id": e.ID, "client_id": e.ClientID, "client_hostname": e.ClientHostName,
		"request": e.Request, "response": e.Response,
	})
}
func (AuditLog) List(context.Context, *query.ListOptions) ([]*auditlog.Entry, error) { return nil, nil }
func (AuditLog) Count(context.Context, *query.ListOptions) (int, error)              { return 0, nil }
func (AuditLog) Close() error                                                        { return nil }

// ---------------------------------------------------------------------------------------------------------------

// NoUsers is the user source of a device process: it has no API, and its database role can't read users.
type NoUsers struct{}

var _ users.Provider = NoUsers{}

func (NoUsers) Type() enums.ProviderSource                        { return enums.ProviderSourceDB }
func (NoUsers) SupportsGroupPermissions() bool                    { return false }
func (NoUsers) GetAll(context.Context) ([]*users.User, error)     { return nil, nil }
func (NoUsers) ListGroups(context.Context) ([]users.Group, error) { return nil, nil }
func (NoUsers) GetGroup(context.Context, string) (users.Group, error) {
	return users.Group{}, ErrNotOnDeviceProcess
}
func (NoUsers) UpdateGroup(context.Context, string, users.Group) error     { return ErrNotOnDeviceProcess }
func (NoUsers) DeleteGroup(context.Context, string) error                  { return ErrNotOnDeviceProcess }
func (NoUsers) GetByUsername(context.Context, string) (*users.User, error) { return nil, nil }
func (NoUsers) Add(context.Context, *users.User) error                     { return ErrNotOnDeviceProcess }
func (NoUsers) Update(context.Context, *users.User, string) error          { return ErrNotOnDeviceProcess }
func (NoUsers) Delete(context.Context, string) error                       { return ErrNotOnDeviceProcess }
