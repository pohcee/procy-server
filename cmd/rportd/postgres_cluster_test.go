package main

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/peer"
)

func TestClusterCertIssuesRoleBoundCertificate(t *testing.T) {
	dir := t.TempDir()
	ca := &clusterCertFlags{cert: filepath.Join(dir, "ca.crt"), key: filepath.Join(dir, "ca.key"), validity: time.Hour}
	require.NoError(t, runClusterCA(ca))
	assert.Error(t, runClusterCA(ca), "existing files are not overwritten")

	o := &clusterCertFlags{
		caCert: ca.cert, caKey: ca.key, node: "node-1", roles: []string{"api,device"}, trustDomain: "rport",
		cert: filepath.Join(dir, "n.crt"), key: filepath.Join(dir, "n.key"), validity: time.Hour,
	}
	require.NoError(t, runClusterCert(o))
	b, err := os.ReadFile(o.cert)
	require.NoError(t, err)
	blk, _ := pem.Decode(b)
	cert, err := x509.ParseCertificate(blk.Bytes)
	require.NoError(t, err)
	id, err := peer.IdentityOf(cert, "rport")
	require.NoError(t, err)
	assert.Equal(t, "node-1", id.NodeID)
	assert.True(t, id.Roles[peer.RoleAPI] && id.Roles[peer.RoleDevice])
	st, err := os.Stat(o.key)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())

	o.roles, o.cert, o.key = []string{"root"}, filepath.Join(dir, "x.crt"), filepath.Join(dir, "x.key")
	assert.Error(t, runClusterCert(o))
}
