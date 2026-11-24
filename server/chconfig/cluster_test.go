package chconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func clusterConfig(role string, cl ClusterConfig) *Config {
	c := postgresConfig()
	c.Storage.Role = role
	c.Cluster = cl
	return c
}

func certFiles() ClusterConfig {
	return ClusterConfig{NodeID: "n1", CAFile: "/etc/rport/ca.crt", CertFile: "/etc/rport/node.crt", KeyFile: "/etc/rport/node.key"}
}

func TestClusterNeedsPostgres(t *testing.T) {
	err := validate(&Config{Server: defaultValidMinServerConfig, Cluster: ClusterConfig{PeerAddress: "10.0.0.1:7443"}})
	assert.ErrorContains(t, err, `[cluster] is set but [storage] backend is not "postgres"`)
	require.NoError(t, validate(&Config{Server: defaultValidMinServerConfig}))
}

func TestClusterDefaults(t *testing.T) {
	osHostname = func() (string, error) { return "node-7", nil }
	t.Cleanup(func() { osHostname = os.Hostname })
	c := clusterConfig("", ClusterConfig{})
	require.NoError(t, validate(c))
	assert.Equal(t, "node-7", c.Cluster.NodeID)
	assert.Equal(t, DefaultTrustDomain, c.Cluster.TrustDomain)
	assert.False(t, c.Cluster.PeerEnabled())

	assert.ErrorContains(t, validate(clusterConfig("", ClusterConfig{NodeID: "bad node"})), "invalid [cluster] node_id")
}

func TestClusterPeerCertificates(t *testing.T) {
	cl := certFiles()
	cl.PeerAddress = "10.0.0.5:7443"
	c := clusterConfig(StorageRoleAll, cl)
	require.NoError(t, validate(c))
	assert.True(t, c.Cluster.PeerEnabled())
	assert.Equal(t, "10.0.0.5:7443", c.Cluster.PeerListenAddress())

	// a process with a client listener must say where it is reached
	err := validate(clusterConfig(StorageRoleAll, certFiles()))
	assert.ErrorContains(t, err, "[cluster] peer_address is required")
	// an api process has no peer listener
	err = validate(clusterConfig(StorageRoleAPI, cl))
	assert.ErrorContains(t, err, "not [storage] role = \"api\"")
	require.NoError(t, validate(clusterConfig(StorageRoleAPI, certFiles())))

	partial := certFiles()
	partial.KeyFile = ""
	assert.ErrorContains(t, validate(clusterConfig(StorageRoleAPI, partial)), "[cluster] key_file is required")

	bad := certFiles()
	bad.TrustDomain = "Not A Domain"
	assert.ErrorContains(t, validate(clusterConfig(StorageRoleAPI, bad)), "invalid [cluster] trust_domain")

	noPeer := ClusterConfig{NodeID: "n1", PeerAddress: "10.0.0.5:7443"}
	assert.ErrorContains(t, validate(clusterConfig(StorageRoleAll, noPeer)), "peer calls are off")
}

func TestClusterPeerSecretNeedsInsecureDev(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "peer")
	require.NoError(t, os.WriteFile(secret, []byte("a-long-enough-dev-secret\n"), 0o600))

	cl := ClusterConfig{NodeID: "n1", PeerSecretFile: secret}
	err := validate(clusterConfig(StorageRoleAPI, cl))
	assert.ErrorContains(t, err, "peer_secret_file is for development only")

	cl.InsecureDev = true
	c := clusterConfig(StorageRoleAPI, cl)
	require.NoError(t, validate(c))
	assert.Equal(t, "a-long-enough-dev-secret", c.Cluster.PeerSecret)

	both := cl
	both.CAFile = "/etc/rport/ca.crt"
	assert.ErrorContains(t, validate(clusterConfig(StorageRoleAPI, both)), "expected one of them")

	short := filepath.Join(t.TempDir(), "short")
	require.NoError(t, os.WriteFile(short, []byte("short"), 0o600))
	assert.ErrorContains(t, validate(clusterConfig(StorageRoleAPI, ClusterConfig{NodeID: "n1", PeerSecretFile: short, InsecureDev: true})), "at least 16")

	certs := certFiles()
	certs.InsecureDev = true
	assert.ErrorContains(t, validate(clusterConfig(StorageRoleAPI, certs)), "insecure_dev is set but peer_secret_file is not")
}
