package chconfig

import (
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
)

// ClusterConfig is the [cluster] section: how a node sharing a Postgres database with other nodes identifies itself
// and how its processes reach each other. Postgres only.
//
// Peer calls (an api process passing work on a client to the device process holding the client) are on when the
// certificate files (ca_file, cert_file, key_file) or, for development only, peer_secret_file are set.
type ClusterConfig struct {
	// NodeID names the node among those sharing the database; defaults to the hostname. Both processes of a node
	// (api and device) use the same node_id.
	NodeID string `mapstructure:"node_id"`
	// Region is reported with the node in the registry (informational).
	Region string `mapstructure:"region"`
	// PeerAddress is the host:port other processes reach this process's peer listener at. Set on processes with a
	// client listener (storage role "all" or "device").
	PeerAddress string `mapstructure:"peer_address"`
	// PeerListen is the address the peer listener binds; defaults to PeerAddress.
	PeerListen string `mapstructure:"peer_listen"`
	// TrustDomain is the trust domain in the role URIs of the peer certificates
	// (spiffe://<trust_domain>/role/<role>/node/<node_id>); default "rport".
	TrustDomain string `mapstructure:"trust_domain"`
	// CAFile is the PEM file of the CA that issues the peer certificates.
	CAFile string `mapstructure:"ca_file"`
	// CertFile and KeyFile are this process's peer certificate and key (PEM).
	CertFile string `mapstructure:"cert_file"`
	KeyFile  string `mapstructure:"key_file"`
	// PeerSecretFile holds a shared secret that replaces the certificates in development: plain HTTP, and any
	// process holding the secret can pose as any role. Refused unless InsecureDev is true.
	PeerSecretFile string `mapstructure:"peer_secret_file"`
	InsecureDev    bool   `mapstructure:"insecure_dev"`

	// PeerSecret is the secret read from PeerSecretFile.
	PeerSecret string `mapstructure:"-"`
}

// DefaultTrustDomain is the trust domain of the peer certificates when [cluster] trust_domain is empty.
const DefaultTrustDomain = "rport"

var (
	nodeIDPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	trustDomainPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
)

// osHostname is os.Hostname, replaced in tests.
var osHostname = os.Hostname

// PeerEnabled reports whether this process makes or answers peer calls.
func (cl ClusterConfig) PeerEnabled() bool {
	return cl.CAFile != "" || cl.CertFile != "" || cl.KeyFile != "" || cl.PeerSecretFile != ""
}

// PeerListenAddress is the address the peer listener binds.
func (cl ClusterConfig) PeerListenAddress() string {
	if cl.PeerListen != "" {
		return cl.PeerListen
	}
	return cl.PeerAddress
}

func (c *Config) parseAndValidateCluster() error {
	cl := &c.Cluster
	if !c.Storage.IsPostgres() {
		if *cl != (ClusterConfig{}) {
			return errors.New(`[cluster] is set but [storage] backend is not "postgres"`)
		}
		return nil
	}
	if err := cl.parseAndValidateNodeID(); err != nil {
		return err
	}
	if cl.TrustDomain == "" {
		cl.TrustDomain = DefaultTrustDomain
	}
	if !trustDomainPattern.MatchString(cl.TrustDomain) {
		return fmt.Errorf("invalid [cluster] trust_domain %q: expected lower-case letters, digits, '.' and '-'", cl.TrustDomain)
	}
	if len(cl.Region) > 63 {
		return errors.New("[cluster] region is longer than 63 characters")
	}
	if !cl.PeerEnabled() {
		for _, kv := range [][2]string{{"peer_address", cl.PeerAddress}, {"peer_listen", cl.PeerListen}} {
			if kv[1] != "" {
				return fmt.Errorf("[cluster] %s is set but peer calls are off: set ca_file, cert_file and key_file", kv[0])
			}
		}
		if cl.InsecureDev {
			return errors.New("[cluster] insecure_dev is set but peer_secret_file is not")
		}
		return nil
	}

	if cl.PeerSecretFile != "" {
		if cl.CAFile != "" || cl.CertFile != "" || cl.KeyFile != "" {
			return errors.New("[cluster] peer_secret_file and the certificate files are both set: expected one of them")
		}
		if !cl.InsecureDev {
			return errors.New("[cluster] peer_secret_file is for development only: any process holding it can pose as an api process; use ca_file, cert_file and key_file, or set insecure_dev = true")
		}
		b, err := os.ReadFile(cl.PeerSecretFile)
		if err != nil {
			return fmt.Errorf("[cluster] peer_secret_file: %w", err)
		}
		cl.PeerSecret = strings.TrimSpace(string(b))
		if len(cl.PeerSecret) < 16 {
			return errors.New("[cluster] peer_secret_file must hold at least 16 characters")
		}
	} else {
		for _, kv := range [][2]string{{"ca_file", cl.CAFile}, {"cert_file", cl.CertFile}, {"key_file", cl.KeyFile}} {
			if kv[1] == "" {
				return fmt.Errorf("[cluster] %s is required with the other peer certificate files", kv[0])
			}
		}
		if cl.InsecureDev {
			return errors.New("[cluster] insecure_dev is set but peer_secret_file is not")
		}
	}

	hasClientListener := c.Storage.Role != StorageRoleAPI
	switch {
	case hasClientListener && cl.PeerAddress == "":
		return errors.New("[cluster] peer_address is required on a process with a client listener when peer calls are on: api processes reach its clients there")
	case !hasClientListener && (cl.PeerAddress != "" || cl.PeerListen != ""):
		return errors.New(`[cluster] peer_address and peer_listen are for processes with a client listener, not [storage] role = "api"`)
	}
	for _, kv := range [][2]string{{"peer_address", cl.PeerAddress}, {"peer_listen", cl.PeerListen}} {
		if kv[1] == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(kv[1]); err != nil {
			return fmt.Errorf("invalid [cluster] %s %q: %w", kv[0], kv[1], err)
		}
	}
	return nil
}

func (cl *ClusterConfig) parseAndValidateNodeID() error {
	if cl.NodeID == "" {
		host, err := osHostname()
		if err != nil {
			return fmt.Errorf("[cluster] node_id is empty and the hostname can't be read: %w", err)
		}
		if host == "" {
			return errors.New("[cluster] node_id is empty and the hostname is empty: set node_id")
		}
		if !nodeIDPattern.MatchString(host) {
			return fmt.Errorf("invalid [cluster] node_id %q (the hostname): expected up to 63 letters, digits, '.', '_' or '-', starting with a letter or digit; set node_id", host)
		}
		cl.NodeID = host
		return nil
	}
	if !nodeIDPattern.MatchString(cl.NodeID) {
		return fmt.Errorf("invalid [cluster] node_id %q: expected up to 63 letters, digits, '.', '_' or '-', starting with a letter or digit", cl.NodeID)
	}
	return nil
}
