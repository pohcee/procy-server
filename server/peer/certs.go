package peer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"strings"
	"time"
)

// Role is what a process of the cluster may do in peer calls. It is bound to the process's certificate as a URI
// SAN spiffe://<trust domain>/role/<role>/node/<node id>; a certificate may carry several (a process with storage role
// "all" has both).
type Role string

const (
	// RoleAPI makes peer calls (an api process).
	RoleAPI Role = "api"
	// RoleDevice answers them (a device process).
	RoleDevice Role = "device"
)

// RoleURI is the URI SAN binding role and nodeID to a certificate.
func RoleURI(trustDomain string, role Role, nodeID string) *url.URL {
	return &url.URL{Scheme: "spiffe", Host: trustDomain, Path: "/role/" + string(role) + "/node/" + nodeID}
}

// Identity is what a verified peer certificate says about its holder.
type Identity struct {
	NodeID string
	Roles  map[Role]bool
}

// IdentityOf reads the role URIs of trustDomain from cert. URIs of other trust domains are ignored; a certificate
// naming two different nodes is refused.
func IdentityOf(cert *x509.Certificate, trustDomain string) (Identity, error) {
	id := Identity{Roles: map[Role]bool{}}
	for _, u := range cert.URIs {
		if u.Scheme != "spiffe" || u.Host != trustDomain {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		if len(parts) != 4 || parts[0] != "role" || parts[2] != "node" || parts[3] == "" {
			continue
		}
		if id.NodeID != "" && id.NodeID != parts[3] {
			return Identity{}, errors.New("peer certificate names more than one node")
		}
		id.NodeID = parts[3]
		id.Roles[Role(parts[1])] = true
	}
	if id.NodeID == "" {
		return Identity{}, fmt.Errorf("peer certificate has no role of trust domain %q", trustDomain)
	}
	return id, nil
}

func newKey() (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// NewCA returns a self-signed CA certificate and its key (PEM), for development and tests; production CAs are
// the deployment's.
func NewCA(commonName string, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	key, keyPEM, err := newKey()
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM, nil
}

// NodeCertRequest describes a node certificate to issue.
type NodeCertRequest struct {
	TrustDomain string
	NodeID      string
	Roles       []Role
	// Hosts are extra DNS names or IP addresses (informational: peers verify the role URI, not the host name).
	Hosts    []string
	Validity time.Duration
}

// IssueNodeCert issues a certificate (and a new key, PEM) for a process of the cluster, signed by the CA.
func IssueNodeCert(caCertPEM, caKeyPEM []byte, req NodeCertRequest) (certPEM, keyPEM []byte, err error) {
	if req.NodeID == "" || len(req.Roles) == 0 || req.TrustDomain == "" {
		return nil, nil, errors.New("a node certificate needs a trust domain, a node id and at least one role")
	}
	caCert, caKey, err := parseCA(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, err
	}
	key, keyPEM, err := newKey()
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: sn,
		Subject:      pkix.Name{CommonName: req.NodeID},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(req.Validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	for _, r := range req.Roles {
		if r != RoleAPI && r != RoleDevice {
			return nil, nil, fmt.Errorf("unknown role %q: expected %q or %q", r, RoleAPI, RoleDevice)
		}
		tmpl.URIs = append(tmpl.URIs, RoleURI(req.TrustDomain, r, req.NodeID))
	}
	for _, h := range req.Hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM, nil
}

func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, any, error) {
	b, _ := pem.Decode(certPEM)
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, nil, errors.New("CA certificate: no PEM certificate")
	}
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("CA certificate: %w", err)
	}
	if !cert.IsCA {
		return nil, nil, errors.New("CA certificate is not a CA")
	}
	k, _ := pem.Decode(keyPEM)
	if k == nil {
		return nil, nil, errors.New("CA key: no PEM block")
	}
	var key any
	switch k.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(k.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(k.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(k.Bytes)
	default:
		err = fmt.Errorf("unsupported key type %q", k.Type)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("CA key: %w", err)
	}
	return cert, key, nil
}
