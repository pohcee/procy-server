package peer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pohcee/procy-server/server/dispatch"
	"github.com/pohcee/procy-server/share/comm"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/models"
	"github.com/pohcee/procy-server/share/tenant"
	"github.com/pohcee/procy-server/share/ws"
)

// maxCallBody bounds the JSON body of a peer call (uploads stream their file separately).
const maxCallBody = 4 << 20

// ServerOptions are what a peer listener needs.
type ServerOptions struct {
	Auth *Auth
	// Local does the work on the clients connected to this process.
	Local dispatch.Dispatcher
	// UIJobStreams is the cache the client listener writes job output and results to.
	UIJobStreams *ws.WebSocketCache
	// UploadDir is where uploaded files are kept while the client fetches them; files left there by an earlier run
	// are removed at Start.
	UploadDir string
	// MaxUploadBytes bounds an uploaded file; 0 means no bound.
	MaxUploadBytes int64
	Log            *logger.Logger
}

// Server is the peer listener of a device process: it accepts work on its clients from api processes.
type Server struct {
	o      ServerOptions
	relay  *relay
	http   *http.Server
	ln     net.Listener
	served chan error

	closeOnce sync.Once
	closeErr  error
}

// NewServer returns a peer listener; Start opens it.
func NewServer(o ServerOptions) (*Server, error) {
	switch {
	case o.Auth == nil:
		return nil, errors.New("peer server: Auth is required")
	case o.Local == nil:
		return nil, errors.New("peer server: Local is required")
	case o.UIJobStreams == nil:
		return nil, errors.New("peer server: UIJobStreams is required")
	case o.UploadDir == "":
		return nil, errors.New("peer server: UploadDir is required")
	}
	if o.Log == nil {
		o.Log = logger.NewLogger("peer", logger.LogOutput{}, logger.LogLevelError)
	}
	s := &Server{o: o, relay: newRelay(o.UIJobStreams, o.Log)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+pathCall, s.handleCall)
	mux.HandleFunc("POST "+pathRunJob, s.handleRunJob)
	mux.HandleFunc("POST "+pathUpload, s.handleUpload)
	s.http = &http.Server{
		Handler:           s.authorized(mux),
		TLSConfig:         o.Auth.serverTLS(),
		ReadHeaderTimeout: 10 * time.Second,
		// refused handshakes and the like
		ErrorLog: log.New(debugWriter{o.Log}, "", 0),
	}
	return s, nil
}

type debugWriter struct{ l *logger.Logger }

func (w debugWriter) Write(p []byte) (int, error) {
	w.l.Debugf("%s", strings.TrimSpace(string(p)))
	return len(p), nil
}

// Handler is the peer API with authorization, for tests that serve it themselves.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Start listens on addr and serves until Close.
func (s *Server) Start(addr string) error {
	if left, _ := filepath.Glob(filepath.Join(s.o.UploadDir, "peer-*")); len(left) > 0 {
		for _, f := range left {
			_ = os.Remove(f)
		}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("peer listener: %w", err)
	}
	s.ln = ln
	s.served = make(chan error, 1)
	go func() {
		var err error
		if s.http.TLSConfig != nil {
			err = s.http.ServeTLS(ln, "", "")
		} else {
			err = s.http.Serve(ln)
		}
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		s.served <- err
	}()
	s.o.Log.Infof("peer listener on %s", ln.Addr())
	return nil
}

// Addr is the address the listener listens on (after Start).
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close stops the listener; streams in progress are cut. Calling it again does nothing.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		if s.ln == nil {
			return
		}
		s.closeErr = s.http.Close()
		if serr := <-s.served; serr != nil && s.closeErr == nil {
			s.closeErr = serr
		}
	})
	return s.closeErr
}

// authorized lets only api processes call.
func (s *Server) authorized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.o.Auth.authorize(r, RoleAPI); err != nil {
			status := http.StatusUnauthorized
			if errors.Is(err, errForbidden) {
				status = http.StatusForbidden
			}
			s.o.Log.Infof("peer call %s from %s refused: %v", r.URL.Path, r.RemoteAddr, err)
			http.Error(w, http.StatusText(status), status)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (t target) ctx(parent context.Context) (context.Context, error) {
	if t.Tenant == "" || t.ClientID == "" {
		return nil, errors.New("peer call without tenant or client id")
	}
	return tenant.WithID(parent, t.Tenant), nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	var req callRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxCallBody)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx, err := req.ctx(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	result, err := s.call(ctx, req)
	resp := callResponse{Error: toWire(err)}
	if err == nil && result != nil {
		b, merr := json.Marshal(result)
		if merr != nil {
			resp.Error = toWire(merr)
		} else {
			resp.Result = b
		}
	}
	writeJSON(w, resp)
}

func (s *Server) call(ctx context.Context, req callRequest) (any, error) {
	d, id := s.o.Local, req.ClientID
	decode := func(v any) error {
		if err := json.Unmarshal(req.Args, v); err != nil {
			return fmt.Errorf("peer call %s: bad arguments: %w", req.Op, err)
		}
		return nil
	}
	switch req.Op {
	case opIsTunnelAllowed:
		var a remoteArgs
		if err := decode(&a); err != nil {
			return nil, err
		}
		return d.IsTunnelAllowed(ctx, id, a.Remote)
	case opCheckPort:
		var a comm.CheckPortRequest
		if err := decode(&a); err != nil {
			return nil, err
		}
		return d.CheckPort(ctx, id, &a)
	case opStartTunnels:
		var remotes []*models.Remote
		if err := decode(&remotes); err != nil {
			return nil, err
		}
		tunnels, err := d.StartTunnels(ctx, id, remotes)
		if err != nil {
			return nil, err
		}
		return tunnels, nil
	case opCloseTunnel:
		var a tunnelArgs
		if err := decode(&a); err != nil {
			return nil, err
		}
		return nil, d.CloseTunnel(ctx, id, a.TunnelID, a.Force)
	case opSetTunnelACL:
		var a tunnelArgs
		if err := decode(&a); err != nil {
			return nil, err
		}
		return nil, d.SetTunnelACL(ctx, id, a.TunnelID, a.ACL)
	case opRefreshUpdatesStatus:
		return nil, d.RefreshUpdatesStatus(ctx, id)
	case opUpdateAttributes:
		var a models.Attributes
		if err := decode(&a); err != nil {
			return nil, err
		}
		return nil, d.UpdateAttributes(ctx, id, a)
	default:
		return nil, fmt.Errorf("unknown peer call %q", req.Op)
	}
}

// runJobStreamGrace is how long a run-job stream waits for the job's result after the job's own timeout.
const runJobStreamGrace = 2 * time.Minute

func (s *Server) handleRunJob(w http.ResponseWriter, r *http.Request) {
	var req runJobRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxCallBody)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx, err := req.ctx(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var job models.Job
	if err := json.Unmarshal(req.Job, &job); err != nil || job.JID == "" {
		http.Error(w, "bad job", http.StatusBadRequest)
		return
	}
	key := job.JID
	if job.MultiJobID != nil {
		key = *job.MultiJobID
	}

	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	send := func(f frame) bool {
		if err := enc.Encode(f); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	w.Header().Set("Content-Type", "application/x-ndjson")

	// relayed before the job is sent, so no output is missed
	sink, closeRelay := s.relay.open(key, job.JID)
	defer closeRelay()

	resp, err := s.o.Local.RunJob(ctx, req.ClientID, &job)
	if err != nil {
		send(frame{Type: frameError, Error: toWire(err)})
		return
	}
	started, err := json.Marshal(resp)
	if err != nil || !send(frame{Type: frameStarted, Data: started}) {
		return
	}

	limit := time.Duration(job.TimeoutSec)*time.Second + runJobStreamGrace
	if job.TimeoutSec <= 0 {
		limit = time.Hour
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for {
		select {
		case m := <-sink.ch:
			typ := frameOutput
			if m.final {
				typ = frameResult
			}
			if !send(frame{Type: typ, Data: m.data}) || m.final {
				return
			}
		case <-timer.C:
			s.o.Log.Infof("job %s: no result within %s; the api process gets it from the database", job.JID, limit)
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	var req uploadRequest
	if err := json.Unmarshal([]byte(r.Header.Get(headerUpload)), &req); err != nil {
		http.Error(w, "bad upload header: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx, err := req.ctx(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var file models.UploadedFile
	if err := json.Unmarshal(req.File, &file); err != nil {
		http.Error(w, "bad upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	body := io.Reader(r.Body)
	if s.o.MaxUploadBytes > 0 {
		body = http.MaxBytesReader(w, r.Body, s.o.MaxUploadBytes)
	}
	path, err := s.storeUpload(body)
	if err != nil {
		writeJSON(w, uploadResponse{Error: toWire(err)})
		return
	}
	defer func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			s.o.Log.Errorf("failed to delete uploaded file %s: %v", path, err)
		}
	}()
	// the client fetches the file from here, over SFTP, while the local dispatcher grants it
	file.SourceFilePath = path
	resp, err := s.o.Local.Upload(ctx, req.ClientID, &file)
	out := uploadResponse{Error: toWire(err)}
	if resp != nil {
		out.Response, _ = json.Marshal(resp)
	}
	writeJSON(w, out)
}

func (s *Server) storeUpload(body io.Reader) (string, error) {
	if err := os.MkdirAll(s.o.UploadDir, 0o700); err != nil {
		return "", fmt.Errorf("peer upload: %w", err)
	}
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	path := filepath.Join(s.o.UploadDir, "peer-"+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("peer upload: %w", err)
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("peer upload: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("peer upload: %w", err)
	}
	return path, nil
}
