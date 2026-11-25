package peer

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	errors2 "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/server/clients/clienttunnel"
	"github.com/openrport/openrport/server/dispatch"
	"github.com/openrport/openrport/server/jobrunner"
	"github.com/openrport/openrport/share/comm"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/tenant"
	"github.com/openrport/openrport/share/ws"
)

const td = "test.cluster"

var testLog = logger.NewLogger("test", logger.LogOutput{}, logger.LogLevelError)

type pki struct {
	dir           string
	caCert, caKey []byte
	caFile        string
	otherCACert   []byte
	otherCAKeyPEM []byte
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	dir := t.TempDir()
	caCert, caKey, err := NewCA("test CA", time.Hour)
	require.NoError(t, err)
	p := &pki{dir: dir, caCert: caCert, caKey: caKey, caFile: filepath.Join(dir, "ca.crt")}
	require.NoError(t, os.WriteFile(p.caFile, caCert, 0o600))
	p.otherCACert, p.otherCAKeyPEM, err = NewCA("other CA", time.Hour)
	require.NoError(t, err)
	return p
}

// auth issues a certificate for node with roles (by the test CA, or another CA) and returns its Auth.
func (p *pki) auth(t *testing.T, node string, otherCA bool, roles ...Role) *Auth {
	t.Helper()
	caCert, caKey := p.caCert, p.caKey
	if otherCA {
		caCert, caKey = p.otherCACert, p.otherCAKeyPEM
	}
	cert, key, err := IssueNodeCert(caCert, caKey, NodeCertRequest{TrustDomain: td, NodeID: node, Roles: roles, Validity: time.Hour})
	require.NoError(t, err)
	name := filepath.Join(p.dir, node+string(roles[0]))
	if otherCA {
		name += "-other"
	}
	require.NoError(t, os.WriteFile(name+".crt", cert, 0o600))
	require.NoError(t, os.WriteFile(name+".key", key, 0o600))
	a, err := NewTLSAuth(td, p.caFile, name+".crt", name+".key")
	require.NoError(t, err)
	return a
}

func TestCertificateCarriesRoleAndNode(t *testing.T) {
	p := newPKI(t)
	a := p.auth(t, "node-1", false, RoleAPI, RoleDevice)
	id, err := a.Identity()
	require.NoError(t, err)
	assert.Equal(t, "node-1", id.NodeID)
	assert.Equal(t, map[Role]bool{RoleAPI: true, RoleDevice: true}, id.Roles)

	m, err := a.certs.current()
	require.NoError(t, err)
	_, err = IdentityOf(m.leaf, "other.domain")
	assert.Error(t, err, "roles of another trust domain don't count")

	_, _, err = IssueNodeCert(p.caCert, p.caKey, NodeCertRequest{TrustDomain: td, NodeID: "n", Roles: []Role{"admin"}, Validity: time.Hour})
	assert.Error(t, err)
}

// fakeLocal is a local dispatcher that records calls; RunJob writes output and the result to the UI job stream
// as the client listener does.
type fakeLocal struct {
	streams  *ws.WebSocketCache
	mu       sync.Mutex
	tenants  []tenant.ID
	upload   []byte
	err      error
	noResult bool
}

func (f *fakeLocal) seen(ctx context.Context) {
	tid, _ := tenant.FromContext(ctx)
	f.mu.Lock()
	f.tenants = append(f.tenants, tid)
	f.mu.Unlock()
}

func (f *fakeLocal) RunJob(ctx context.Context, _ string, job *models.Job) (*comm.RunCmdResponse, error) {
	f.seen(ctx)
	if f.err != nil {
		return nil, f.err
	}
	key := job.JID
	if job.MultiJobID != nil {
		key = *job.MultiJobID
	}
	if f.noResult {
		return &comm.RunCmdResponse{Pid: 1}, nil
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		s := f.streams.Get(key)
		if s == nil {
			return
		}
		_ = s.WriteNonFinalJSON(map[string]any{"jid": job.JID, "result": map[string]string{"stdout": "hello "}})
		_ = s.WriteNonFinalJSON(map[string]any{"jid": "other-job", "result": map[string]string{"stdout": "not mine"}})
		done := *job
		done.Status = models.JobStatusSuccessful
		done.Result = &models.JobResult{StdOut: "hello world"}
		b, _ := json.Marshal(done)
		_ = s.WriteMessage(1, b)
	}()
	return &comm.RunCmdResponse{Pid: 42, StartedAt: time.Now()}, nil
}

func (f *fakeLocal) Upload(ctx context.Context, _ string, file *models.UploadedFile) (*models.UploadResponse, error) {
	f.seen(ctx)
	b, err := os.ReadFile(file.SourceFilePath)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.upload = b
	f.mu.Unlock()
	return &models.UploadResponse{UploadResponseShort: models.UploadResponseShort{ID: file.ID, Filepath: file.DestinationPath, SizeBytes: int64(len(b))}, Status: "success"}, nil
}

func (f *fakeLocal) IsTunnelAllowed(ctx context.Context, _, remote string) (bool, error) {
	f.seen(ctx)
	return remote == "127.0.0.1:22", f.err
}

func (f *fakeLocal) CheckPort(ctx context.Context, _ string, _ *comm.CheckPortRequest) (*comm.CheckPortResponse, error) {
	f.seen(ctx)
	if f.err != nil {
		return nil, f.err
	}
	return &comm.CheckPortResponse{Open: true}, nil
}

func (f *fakeLocal) StartTunnels(ctx context.Context, _ string, remotes []*models.Remote) ([]*clienttunnel.Tunnel, error) {
	f.seen(ctx)
	out := make([]*clienttunnel.Tunnel, 0, len(remotes))
	for i, r := range remotes {
		out = append(out, &clienttunnel.Tunnel{ID: string(rune('1' + i)), Remote: *r, TunnelHost: "node-b.example"})
	}
	return out, f.err
}

func (f *fakeLocal) CloseTunnel(ctx context.Context, _, _ string, _ bool) error {
	f.seen(ctx)
	return f.err
}
func (f *fakeLocal) SetTunnelACL(ctx context.Context, _, _ string, _ *string) error {
	f.seen(ctx)
	return f.err
}
func (f *fakeLocal) RefreshUpdatesStatus(ctx context.Context, _ string) error {
	f.seen(ctx)
	return f.err
}
func (f *fakeLocal) UpdateAttributes(ctx context.Context, _ string, _ models.Attributes) error {
	f.seen(ctx)
	return f.err
}

type staticLocator struct{ loc *Location }

func (s staticLocator) Locate(context.Context, string) (*Location, error) { return s.loc, nil }

// capture is a ws.Conn recording what is written to a UI websocket.
type capture struct {
	mu     sync.Mutex
	writes []string
}

func (c *capture) NextReader() (int, io.Reader, error) { return 0, nil, io.EOF }
func (c *capture) ReadMessage() (int, []byte, error)   { return 0, nil, io.EOF }
func (c *capture) WriteMessage(t int, b []byte) error {
	if t == websocket.CloseMessage {
		return nil // the end of the stream
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, "final:"+string(b))
	return nil
}
func (c *capture) WriteJSON(v interface{}) error {
	b, _ := json.Marshal(v)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, string(b))
	return nil
}
func (c *capture) Close() error { return nil }
func (c *capture) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.writes...)
}

type pair struct {
	local      *fakeLocal
	dispatcher *Dispatcher
	streams    *ws.WebSocketCache // api side
	done       *jobrunner.DoneChannels
	addr       string
	srv        *Server
}

func startPair(t *testing.T, deviceAuth, apiAuth *Auth) *pair {
	t.Helper()
	deviceStreams := ws.NewWebSocketCache()
	local := &fakeLocal{streams: &deviceStreams}
	srv, err := NewServer(ServerOptions{Auth: deviceAuth, Local: local, UIJobStreams: &deviceStreams, UploadDir: t.TempDir()})
	require.NoError(t, err)
	require.NoError(t, srv.Start("127.0.0.1:0"))
	t.Cleanup(func() { _ = srv.Close() })

	apiStreams := ws.NewWebSocketCache()
	done := jobrunner.NewDoneChannels()
	d := NewDispatcher(staticLocator{&Location{NodeID: "node-b", PeerAddress: srv.Addr()}}, NewClient(apiAuth), "node-a", nil)
	d.Bind(Binding{UIJobStreams: &apiStreams, JobsDone: done})
	return &pair{local: local, dispatcher: d, streams: &apiStreams, done: done, addr: srv.Addr(), srv: srv}
}

func ctxA() context.Context { return tenant.WithID(context.Background(), "acme") }

func TestRunJobStreamsOutputAndResult(t *testing.T) {
	p := newPKI(t)
	pr := startPair(t, p.auth(t, "node-b", false, RoleDevice), p.auth(t, "node-a", false, RoleAPI))

	ui := &capture{}
	pr.streams.Set("job-1", ws.NewConcurrentWebSocket(ui, testLog))
	resp, err := pr.dispatcher.RunJob(ctxA(), "c1", &models.Job{JID: "job-1", ClientID: "c1", TimeoutSec: 10})
	require.NoError(t, err)
	assert.Equal(t, 42, resp.Pid)
	require.Eventually(t, func() bool { return len(ui.all()) == 2 }, 5*time.Second, 20*time.Millisecond)
	w := ui.all()
	assert.JSONEq(t, `{"jid":"job-1","result":{"stdout":"hello "}}`, w[0], "live output, only of this job")
	assert.Contains(t, w[1], "final:")
	assert.Contains(t, w[1], "hello world")
	assert.Equal(t, []tenant.ID{"acme"}, pr.local.tenants, "the tenant travels with the call")

	// a sequential multi-client run waiting here gets the result
	multi := "multi-1"
	ch := make(chan *models.Job, 1)
	pr.done.Set(multi, ch)
	_, err = pr.dispatcher.RunJob(ctxA(), "c1", &models.Job{JID: "job-2", MultiJobID: &multi, ClientID: "c1", TimeoutSec: 10})
	require.NoError(t, err)
	select {
	case j := <-ch:
		assert.Equal(t, "job-2", j.JID)
		assert.Equal(t, models.JobStatusSuccessful, j.Status)
	case <-time.After(5 * time.Second):
		t.Fatal("the sequential run was not signaled")
	}
}

func TestPeerErrorsKeepTheirMeaning(t *testing.T) {
	p := newPKI(t)
	pr := startPair(t, p.auth(t, "node-b", false, RoleDevice), p.auth(t, "node-a", false, RoleAPI))

	pr.local.err = comm.NewClientError(errors.New("command not allowed"))
	_, err := pr.dispatcher.RunJob(ctxA(), "c1", &models.Job{JID: "j", ClientID: "c1"})
	var ce *comm.ClientError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, "command not allowed", err.Error())

	pr.local.err = dispatch.ErrClientNotConnected
	_, err = pr.dispatcher.CheckPort(ctxA(), "c1", &comm.CheckPortRequest{HostPort: "x:1"})
	assert.ErrorIs(t, err, dispatch.ErrClientNotConnected)

	pr.local.err = dispatch.ErrTunnelNotFound
	assert.ErrorIs(t, pr.dispatcher.CloseTunnel(ctxA(), "c1", "9", false), dispatch.ErrTunnelNotFound)

	pr.local.err = errors2.APIError{Message: "port busy", HTTPStatus: http.StatusConflict}
	_, err = pr.dispatcher.StartTunnels(ctxA(), "c1", []*models.Remote{{RemoteHost: "127.0.0.1", RemotePort: "22"}})
	var ae errors2.APIError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, http.StatusConflict, ae.HTTPStatus)

	// the device process's refusal at the transfer limit keeps its 403 and code
	pr.local.err = errors2.APIError{Message: "transfer limit reached", HTTPStatus: http.StatusForbidden, ErrCode: "ERR_CODE_TRANSFER_LIMIT"}
	_, err = pr.dispatcher.StartTunnels(ctxA(), "c1", []*models.Remote{{RemoteHost: "127.0.0.1", RemotePort: "22"}})
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, http.StatusForbidden, ae.HTTPStatus)
	assert.Equal(t, "ERR_CODE_TRANSFER_LIMIT", ae.ErrCode)
	assert.Equal(t, "transfer limit reached", ae.Message)

	pr.local.err = nil
	tunnels, err := pr.dispatcher.StartTunnels(ctxA(), "c1", []*models.Remote{{RemoteHost: "127.0.0.1", RemotePort: "22"}})
	require.NoError(t, err)
	require.Len(t, tunnels, 1)
	assert.Equal(t, "node-b.example", tunnels[0].TunnelHost)
	assert.Equal(t, "22", tunnels[0].RemotePort)
	ok, err := pr.dispatcher.IsTunnelAllowed(ctxA(), "c1", "127.0.0.1:22")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.NoError(t, pr.dispatcher.RefreshUpdatesStatus(ctxA(), "c1"))
	assert.NoError(t, pr.dispatcher.UpdateAttributes(ctxA(), "c1", models.Attributes{Tags: []string{"x"}}))
	acl := "10.0.0.1"
	assert.NoError(t, pr.dispatcher.SetTunnelACL(ctxA(), "c1", "1", &acl))

	_, err = pr.dispatcher.RunJob(context.Background(), "c1", &models.Job{JID: "j"})
	assert.ErrorIs(t, err, tenant.ErrMissing)
}

func TestUploadStreamsFile(t *testing.T) {
	p := newPKI(t)
	pr := startPair(t, p.auth(t, "node-b", false, RoleDevice), p.auth(t, "node-a", false, RoleAPI))
	src := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(src, []byte("file content"), 0o600))
	resp, err := pr.dispatcher.Upload(ctxA(), "c1", &models.UploadedFile{ID: "u1", SourceFilePath: src, DestinationPath: "/tmp/x"})
	require.NoError(t, err)
	assert.Equal(t, int64(12), resp.SizeBytes)
	assert.Equal(t, "file content", string(pr.local.upload))
}

func TestNotConnectedAnywhere(t *testing.T) {
	d := NewDispatcher(staticLocator{}, nil, "node-a", nil)
	_, err := d.RunJob(ctxA(), "c1", &models.Job{JID: "j"})
	assert.ErrorIs(t, err, dispatch.ErrClientNotConnected)
}

func TestLocalWhenHeldHere(t *testing.T) {
	streams := ws.NewWebSocketCache()
	local := &fakeLocal{streams: &streams}
	d := NewDispatcher(staticLocator{&Location{NodeID: "node-a"}}, nil, "node-a", nil)
	d.Bind(Binding{Local: local, HasClientListener: true, UIJobStreams: &streams})
	ok, err := d.IsTunnelAllowed(ctxA(), "c1", "127.0.0.1:22")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Len(t, local.tenants, 1)
}

func TestDeviceCertificateCannotDispatch(t *testing.T) {
	p := newPKI(t)
	// a compromised device process calling another device process
	pr := startPair(t, p.auth(t, "node-b", false, RoleDevice), p.auth(t, "node-c", false, RoleDevice))
	_, err := pr.dispatcher.CheckPort(ctxA(), "c1", &comm.CheckPortRequest{HostPort: "x:1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
	assert.Empty(t, pr.local.tenants, "nothing ran")
}

func TestPeerWithoutValidCertificateRefused(t *testing.T) {
	p := newPKI(t)
	device := p.auth(t, "node-b", false, RoleDevice)
	pr := startPair(t, device, p.auth(t, "node-a", true, RoleAPI)) // issued by another CA
	_, err := pr.dispatcher.CheckPort(ctxA(), "c1", &comm.CheckPortRequest{HostPort: "x:1"})
	assert.Error(t, err)
	assert.Empty(t, pr.local.tenants)

	// no client certificate at all
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // test
	resp, err := c.Post("https://"+pr.addr+pathCall, "application/json", nil)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("a call without a client certificate got %s", resp.Status)
	}
	assert.Empty(t, pr.local.tenants)

	// plain HTTP to the TLS listener
	conn, err := net.Dial("tcp", pr.addr)
	require.NoError(t, err)
	conn.Close()
}

func TestAPIProcessChecksDeviceNode(t *testing.T) {
	p := newPKI(t)
	// the registry says node-b, but the listener has node-x's certificate
	pr := startPair(t, p.auth(t, "node-x", false, RoleDevice), p.auth(t, "node-a", false, RoleAPI))
	_, err := pr.dispatcher.CheckPort(ctxA(), "c1", &comm.CheckPortRequest{HostPort: "x:1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `expected "node-b"`)

	// an api certificate posing as a device process
	pr = startPair(t, p.auth(t, "node-b", false, RoleAPI), p.auth(t, "node-a", false, RoleAPI))
	_, err = pr.dispatcher.CheckPort(ctxA(), "c1", &comm.CheckPortRequest{HostPort: "x:1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no "device" role`)
}

func TestInsecureSecretMode(t *testing.T) {
	pr := startPair(t, NewInsecureSecretAuth("a-long-dev-secret-1"), NewInsecureSecretAuth("a-long-dev-secret-1"))
	ok, err := pr.dispatcher.IsTunnelAllowed(ctxA(), "c1", "127.0.0.1:22")
	require.NoError(t, err)
	assert.True(t, ok)

	wrong := startPair(t, NewInsecureSecretAuth("a-long-dev-secret-1"), NewInsecureSecretAuth("another-secret-22"))
	_, err = wrong.dispatcher.IsTunnelAllowed(ctxA(), "c1", "127.0.0.1:22")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}

// fakeStore is a SessionStore in memory: newest session wins.
type fakeStore struct {
	mu       sync.Mutex
	sessions map[string]string // client -> session id
	beats    int
	fail     bool
}

func (s *fakeStore) Heartbeat(context.Context, NodeInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beats++
	return nil
}
func (s *fakeStore) Register(_ context.Context, _ string, sess Session, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("database down")
	}
	s.sessions[sess.ClientID] = sess.SessionID
	return nil
}
func (s *fakeStore) Renew(_ context.Context, _ string, held []Session, _ time.Duration) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var lost []string
	for _, h := range held {
		cur, ok := s.sessions[h.ClientID]
		switch {
		case !ok:
			s.sessions[h.ClientID] = h.SessionID
		case cur != h.SessionID:
			lost = append(lost, h.SessionID)
		}
	}
	return lost, nil
}
func (s *fakeStore) End(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c, cur := range s.sessions {
		if cur == id {
			delete(s.sessions, c)
		}
	}
	return nil
}

func TestTrackerClosesSessionTakenOver(t *testing.T) {
	store := &fakeStore{sessions: map[string]string{}}
	tr := NewTracker(store, NodeInfo{NodeID: "n1"}, time.Minute, time.Hour, nil)
	closed := make(chan struct{}, 1)
	end, err := tr.Connected(ctxA(), "c1", func() { closed <- struct{}{} })
	require.NoError(t, err)
	defer end()

	tr.Tick(context.Background())
	select {
	case <-closed:
		t.Fatal("closed while it holds the newest session")
	default:
	}

	// the client connected to another node
	store.mu.Lock()
	store.sessions["c1"] = "session-on-n2"
	store.mu.Unlock()
	tr.Tick(context.Background())
	select {
	case <-closed:
	default:
		t.Fatal("a session another node took over is closed")
	}
	assert.Equal(t, 2, store.beats)

	_, err = tr.Connected(context.Background(), "c1", nil)
	assert.ErrorIs(t, err, tenant.ErrMissing)
}

func TestTrackerLateRecordDoesNotReplaceNewerSession(t *testing.T) {
	store := &fakeStore{sessions: map[string]string{}, fail: true}
	tr := NewTracker(store, NodeInfo{NodeID: "n1"}, time.Minute, time.Hour, nil)
	closed := make(chan struct{}, 1)
	_, err := tr.Connected(ctxA(), "c1", func() { closed <- struct{}{} })
	require.NoError(t, err)
	// meanwhile the client connected to another node
	store.mu.Lock()
	store.fail = false
	store.sessions["c1"] = "session-on-n2"
	store.mu.Unlock()
	tr.Tick(context.Background())
	assert.Equal(t, "session-on-n2", store.sessions["c1"], "the newer session stays")
	select {
	case <-closed:
	default:
		t.Fatal("the connection whose session was never recorded and is now replaced is closed")
	}
}

func TestTrackerRegistersLaterWhenDatabaseIsDown(t *testing.T) {
	store := &fakeStore{sessions: map[string]string{}, fail: true}
	tr := NewTracker(store, NodeInfo{NodeID: "n1"}, time.Minute, time.Hour, nil)
	end, err := tr.Connected(ctxA(), "c1", nil)
	require.NoError(t, err)
	assert.Empty(t, store.sessions)
	store.mu.Lock()
	store.fail = false
	store.mu.Unlock()
	tr.Tick(context.Background())
	assert.Len(t, store.sessions, 1)
	end()
	assert.Empty(t, store.sessions, "ending removes the session")
}

type storedJobs struct{ job *models.Job }

func (s storedJobs) GetByJID(context.Context, string, string) (*models.Job, error) { return s.job, nil }

// A sequential run waiting for a result that never comes over the stream (the device process went away) gets the
// result the device process saved.
func TestSequentialRunGetsStoredResultWhenStreamBreaks(t *testing.T) {
	p := newPKI(t)
	pr := startPair(t, p.auth(t, "node-b", false, RoleDevice), p.auth(t, "node-a", false, RoleAPI))
	pr.local.noResult = true
	stored := &models.Job{JID: "job-9", Status: models.JobStatusSuccessful}
	pr.dispatcher.Bind(Binding{UIJobStreams: pr.streams, JobsDone: pr.done, Jobs: storedJobs{stored}})

	multi := "multi-9"
	ch := make(chan *models.Job, 1)
	pr.done.Set(multi, ch)
	_, err := pr.dispatcher.RunJob(ctxA(), "c1", &models.Job{JID: "job-9", MultiJobID: &multi, ClientID: "c1", TimeoutSec: 10})
	require.NoError(t, err)
	require.NoError(t, pr.srv.Close())
	select {
	case j := <-ch:
		assert.Equal(t, models.JobStatusSuccessful, j.Status)
	case <-time.After(10 * time.Second):
		t.Fatal("the sequential run was not signaled")
	}
}

// A burst of output larger than the relay's buffer waits for the reader instead of being dropped.
func TestRelayKeepsBurstsOfOutput(t *testing.T) {
	streams := ws.NewWebSocketCache()
	r := newRelay(&streams, testLog)
	sink, closeRelay := r.open("multi", "j1")
	defer closeRelay()
	const n = 1000
	go func() {
		s := streams.Get("multi")
		for i := 0; i < n; i++ {
			_ = s.WriteNonFinalJSON(map[string]any{"jid": "j1", "i": i})
		}
	}()
	for i := 0; i < n; i++ {
		select {
		case m := <-sink.ch:
			var got struct{ I int }
			require.NoError(t, json.Unmarshal(m.data, &got))
			require.Equal(t, i, got.I)
		case <-time.After(5 * time.Second):
			t.Fatalf("output %d was dropped", i)
		}
		if i%100 == 0 {
			time.Sleep(10 * time.Millisecond) // a slow reader
		}
	}
}

// When the stream breaks without a result, the UI stream still gets the job (the stored result, or a failed job)
// as its final write, so it ends.
func TestUIStreamGetsTheStoredResultWhenStreamBreaks(t *testing.T) {
	p := newPKI(t)
	pr := startPair(t, p.auth(t, "node-b", false, RoleDevice), p.auth(t, "node-a", false, RoleAPI))
	pr.local.noResult = true
	stored := &models.Job{JID: "job-7", ClientID: "c1", Status: models.JobStatusSuccessful}
	pr.dispatcher.Bind(Binding{UIJobStreams: pr.streams, JobsDone: pr.done, Jobs: storedJobs{stored}})
	ui := &capture{}
	pr.streams.Set("job-7", ws.NewConcurrentWebSocket(ui, testLog))

	_, err := pr.dispatcher.RunJob(ctxA(), "c1", &models.Job{JID: "job-7", ClientID: "c1", TimeoutSec: 10})
	require.NoError(t, err)
	require.NoError(t, pr.srv.Close())
	require.Eventually(t, func() bool { return len(ui.all()) == 1 }, 10*time.Second, 20*time.Millisecond)
	assert.Contains(t, ui.all()[0], "final:")
	assert.Contains(t, ui.all()[0], models.JobStatusSuccessful)
}
