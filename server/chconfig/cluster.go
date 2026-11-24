package chconfig

import (
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
)

// DefaultTrustDomain is the trust domain of the peer certificates when [cluster] trust_domain is empty.
const DefaultTrustDomain = "rport"

var trustDomainPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

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
		// node_id is checked with [storage]
		if *cl != (ClusterConfig{NodeID: cl.NodeID}) {
			return errors.New(`[cluster] is set but [storage] backend is not "postgres"`)
		}
		return nil
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
