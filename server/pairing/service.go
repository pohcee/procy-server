package pairing

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"

	chshare "github.com/openrport/openrport/share"
	"github.com/openrport/openrport/share/logger"
)

const (
	// PathPrefix is where rportd serves the pairing endpoints:
	//   POST <PathPrefix>          deposit (authenticated, wired up by the API router)
	//   GET  <PathPrefix>/update   update script
	//   GET  <PathPrefix>/{code}   installer script
	PathPrefix = "/pairing"
	// CodeVar is the mux variable holding the pairing code.
	CodeVar = "code"
	// DefaultCodeTTL is how long a pairing code stays valid unless configured.
	DefaultCodeTTL = time.Hour

	// failed installer lookups allowed per remote IP and window
	maxFailedLookups    = 20
	failedLookupsWindow = 10 * time.Minute
	formMaxMem          = 4096
)

var errTooManyCodes = errors.New("too many unexpired pairing codes")

// Options configure the pairing service.
type Options struct {
	// PublicURL is the external base URL of rportd, e.g. https://rport.example.com.
	// Empty = derived from each request (scheme and Host header).
	PublicURL string
	// DownloadsURL is the base URL of the client packages, e.g. https://rport.example.com/downloads.
	DownloadsURL string
	// ConnectURLs are the client connect URLs ([server] url), offered to the update script's
	// server switch. Only public ones are used (PublicConnectURLs).
	ConnectURLs []string
	// CodeTTL is how long a code stays valid (0 = DefaultCodeTTL).
	CodeTTL time.Duration
	// AllowCodeReuse keeps a code valid for further fetches until it expires.
	// By default a code is deleted on its first successful fetch.
	AllowCodeReuse bool
}

// Service serves the pairing endpoints.
type Service struct {
	opts    Options
	log     *logger.Logger
	codes   *store
	limiter *failureLimiter
	now     func() time.Time
}

// NewService creates the pairing service. Codes live in memory only.
func NewService(opts Options, log *logger.Logger) *Service {
	return newService(opts, log, time.Now)
}

func newService(opts Options, log *logger.Logger, now func() time.Time) *Service {
	if opts.CodeTTL <= 0 {
		opts.CodeTTL = DefaultCodeTTL
	}
	opts.PublicURL = strings.TrimRight(opts.PublicURL, "/")
	opts.DownloadsURL = strings.TrimRight(opts.DownloadsURL, "/")
	return &Service{
		opts:    opts,
		log:     log,
		codes:   newStore(now),
		limiter: newFailureLimiter(maxFailedLookups, failedLookupsWindow, now),
		now:     now,
	}
}

// URL returns the public URL of the pairing endpoints for the given request, e.g.
// https://rport.example.com/pairing. This is what rportd reports as its pairing_url.
func (s *Service) URL(r *http.Request) string {
	base := s.opts.PublicURL
	if base == "" {
		base = requestBaseURL(r)
	}
	return base + PathPrefix
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Service) settings(r *http.Request) scriptSettings {
	return scriptSettings{DownloadsURL: s.opts.DownloadsURL, PairingURL: s.URL(r), ConnectURLs: PublicConnectURLs(s.opts.ConnectURLs)}
}

// HandleDeposit stores client credentials and returns a pairing code with install
// one-liners. It must only be reachable for authenticated users; username is used for logging.
func (s *Service) HandleDeposit(w http.ResponseWriter, r *http.Request, username string) {
	d := Deposit{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		// the standalone service also accepted multipart forms
		if err := r.ParseMultipartForm(formMaxMem); err != nil { //nolint:gosec // G120: API body limited by applyMaxBytes
			http.Error(w, "expected a JSON or multipart form body", http.StatusBadRequest)
			return
		}
		d.ConnectURL = r.FormValue("connect_url")
		d.Fingerprint = r.FormValue("fingerprint")
		d.ClientID = r.FormValue("client_id")
		d.Password = r.FormValue("password")
	}
	if err := d.Validate(); err != nil {
		http.Error(w, "input validation failed: "+err.Error(), http.StatusBadRequest)
		return
	}

	code, expires, err := s.codes.put(d, s.opts.CodeTTL)
	if err != nil {
		s.log.Errorf("pairing: storing deposit failed: %v", err)
		http.Error(w, "could not create a pairing code", http.StatusServiceUnavailable)
		return
	}
	s.log.Infof("pairing: user %q created a pairing code for client %q, valid until %s", username, d.ClientID, expires.UTC().Format(time.RFC3339))

	installerURL := s.URL(r) + "/" + code
	resp := &Response{PairingCode: code, Expires: expires.UTC()}
	resp.Installers.Linux = fmt.Sprintf(`curl %s > rport-installer.sh
sudo sh rport-installer.sh`, installerURL)
	resp.Installers.Windows = fmt.Sprintf(`[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$url="%s"
Invoke-WebRequest -Uri $url -OutFile "rport-installer.ps1"
powershell -ExecutionPolicy Bypass -File .\rport-installer.ps1`, installerURL)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.log.Errorf("pairing: writing response: %v", err)
	}
}

// HandleInstaller renders the installer for the code in the mux variable CodeVar.
// No authentication: the random, expiring (and by default single-use) code is the secret.
func (s *Service) HandleInstaller(w http.ResponseWriter, r *http.Request) {
	ip := chshare.RemoteIP(r)
	if s.limiter.blocked(ip) {
		http.Error(w, "# Too many invalid pairing codes, try again later", http.StatusTooManyRequests)
		return
	}
	code := mux.Vars(r)[CodeVar]
	d, found := Deposit{}, false
	if len(code) == CodeLength {
		d, found = s.codes.get(code, !s.opts.AllowCodeReuse)
	}
	if !found {
		s.limiter.fail(ip)
		writeScriptHeaders(w, "")
		w.WriteHeader(http.StatusNotFound)
		// a comment, so "curl ... | sh" fails harmlessly; never echo the requested code
		fmt.Fprintln(w, "# No pairing found for this pairing code. It may have expired or been used already.")
		fmt.Fprintln(w, "exit 1")
		return
	}

	os := clientOS(r)
	body, filename, err := renderInstaller(os, d, s.settings(r))
	if err != nil {
		s.log.Errorf("pairing: rendering installer: %v", err)
		http.Error(w, "# rendering the installer failed", http.StatusInternalServerError)
		return
	}
	s.log.Infof("pairing: %s installer for client %q fetched from %s", os, d.ClientID, ip)
	writeScriptHeaders(w, filename)
	_, _ = w.Write(body)
}

// HandleUpdate renders the client update script (no credentials, no authentication).
func (s *Service) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	body, filename := renderUpdate(clientOS(r), s.settings(r))
	writeScriptHeaders(w, filename)
	_, _ = w.Write(body)
}

func writeScriptHeaders(w http.ResponseWriter, filename string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	if filename != "" {
		h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	}
}
