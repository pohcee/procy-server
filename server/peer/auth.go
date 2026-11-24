package peer

import (
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Auth authenticates peer calls: mutual TLS with certificates bound to the process role (NewTLSAuth), or, in
// development only, a shared secret over plain HTTP (NewInsecureSecretAuth), with which any holder can pose as any
// role.
type Auth struct {
	trustDomain string
	certs       *certSource
	secret      string
}

// NewTLSAuth reads the CA and this process's certificate and key (PEM files). The files are read again when they
// change, so short-lived certificates can be replaced while the process runs.
func NewTLSAuth(trustDomain, caFile, certFile, keyFile string) (*Auth, error) {
	src := &certSource{caFile: caFile, certFile: certFile, keyFile: keyFile}
	if _, err := src.current(); err != nil {
		return nil, err
	}
	return &Auth{trustDomain: trustDomain, certs: src}, nil
}

// NewInsecureSecretAuth authenticates peer calls with a shared secret over plain HTTP. For development only.
func NewInsecureSecretAuth(secret string) *Auth {
	return &Auth{secret: secret}
}

// Identity returns the identity in this process's own certificate (zero in secret mode).
func (a *Auth) Identity() (Identity, error) {
	if a.certs == nil {
		return Identity{}, nil
	}
	m, err := a.certs.current()
	if err != nil {
		return Identity{}, err
	}
	return IdentityOf(m.leaf, a.trustDomain)
}

func (a *Auth) scheme() string {
	if a.certs == nil {
		return "http"
	}
	return "https"
}

// serverTLS is the TLS config of a peer listener: a client certificate issued by the CA is required.
func (a *Auth) serverTLS() *tls.Config {
	if a.certs == nil {
		return nil
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			m, err := a.certs.current()
			if err != nil {
				return nil, err
			}
			return &m.cert, nil
		},
		// verified in VerifyConnection against the current CA file
		ClientAuth: tls.RequireAnyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			_, err := a.verify(cs, x509.ExtKeyUsageClientAuth)
			return err
		},
	}
}

// clientTLS is the TLS config for calls to the device process of node: its certificate must be issued by the CA and
// carry the device role of that node.
func (a *Auth) clientTLS(node string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			m, err := a.certs.current()
			if err != nil {
				return nil, err
			}
			return &m.cert, nil
		},
		// The peer is identified by the role URI in its certificate, not by a host name (peers are addressed by
		// IP); VerifyConnection verifies the chain and the identity.
		InsecureSkipVerify: true, //nolint:gosec // verified in VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			id, err := a.verify(cs, x509.ExtKeyUsageServerAuth)
			if err != nil {
				return err
			}
			if !id.Roles[RoleDevice] {
				return fmt.Errorf("peer %s: certificate has no %q role", id.NodeID, RoleDevice)
			}
			if id.NodeID != node {
				return fmt.Errorf("peer certificate is for node %q, expected %q", id.NodeID, node)
			}
			return nil
		},
	}
}

func (a *Auth) verify(cs tls.ConnectionState, usage x509.ExtKeyUsage) (Identity, error) {
	if len(cs.PeerCertificates) == 0 {
		return Identity{}, errors.New("peer presented no certificate")
	}
	m, err := a.certs.current()
	if err != nil {
		return Identity{}, err
	}
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	if _, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
		Roots: m.roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{usage},
	}); err != nil {
		return Identity{}, fmt.Errorf("peer certificate: %w", err)
	}
	return IdentityOf(cs.PeerCertificates[0], a.trustDomain)
}

// errForbidden is returned by authorize for a caller that is authenticated but may not make the call.
var errForbidden = errors.New("forbidden")

// authorize checks that the caller of a peer call holds role.
func (a *Auth) authorize(r *http.Request, role Role) error {
	if a.certs == nil {
		h := r.Header.Get("Authorization")
		got, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(a.secret)) != 1 {
			return errors.New("missing or wrong peer secret")
		}
		return nil
	}
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return errors.New("no client certificate")
	}
	id, err := IdentityOf(r.TLS.PeerCertificates[0], a.trustDomain)
	if err != nil {
		return err
	}
	if !id.Roles[role] {
		return fmt.Errorf("%w: certificate of node %q has no %q role", errForbidden, id.NodeID, role)
	}
	return nil
}

func (a *Auth) addSecret(req *http.Request) {
	if a.certs == nil {
		req.Header.Set("Authorization", "Bearer "+a.secret)
	}
}

// certSource holds the parsed certificate files and reads them again when one changed (checked at most every few
// seconds).
type certSource struct {
	caFile, certFile, keyFile string

	mu      sync.Mutex
	loaded  *material
	stamp   string
	checked time.Time
}

type material struct {
	cert  tls.Certificate
	leaf  *x509.Certificate
	roots *x509.CertPool
}

const certRecheck = 5 * time.Second

func (s *certSource) current() (*material, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded != nil && time.Since(s.checked) < certRecheck {
		return s.loaded, nil
	}
	s.checked = time.Now()
	stamp, err := s.fileStamp()
	if err != nil {
		if s.loaded != nil {
			return s.loaded, nil // keep the last good files while a deployment replaces them
		}
		return nil, err
	}
	if s.loaded != nil && stamp == s.stamp {
		return s.loaded, nil
	}
	m, err := s.load()
	if err != nil {
		if s.loaded != nil {
			return s.loaded, nil
		}
		return nil, err
	}
	s.loaded, s.stamp = m, stamp
	return m, nil
}

func (s *certSource) fileStamp() (string, error) {
	var b strings.Builder
	for _, f := range []string{s.caFile, s.certFile, s.keyFile} {
		st, err := os.Stat(f)
		if err != nil {
			return "", fmt.Errorf("peer certificates: %w", err)
		}
		fmt.Fprintf(&b, "%d/%d;", st.ModTime().UnixNano(), st.Size())
	}
	return b.String(), nil
}

func (s *certSource) load() (*material, error) {
	caPEM, err := os.ReadFile(s.caFile)
	if err != nil {
		return nil, fmt.Errorf("peer CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("peer CA %s: no PEM certificate", s.caFile)
	}
	cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return nil, fmt.Errorf("peer certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("peer certificate: %w", err)
	}
	cert.Leaf = leaf
	return &material{cert: cert, leaf: leaf, roots: roots}, nil
}
