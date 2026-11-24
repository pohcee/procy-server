package main

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/openrport/openrport/server/storage/postgres/pgtest"
)

// importCounts parses the table "rportd import" prints: entity name → [read, added, present, skipped].
func importCounts(t *testing.T, out string) map[string][4]int {
	t.Helper()
	counts := map[string][4]int{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		var n [4]int
		ok := true
		for i := 0; i < 4; i++ {
			v, err := strconv.Atoi(f[len(f)-4+i])
			if err != nil {
				ok = false
				break
			}
			n[i] = v
		}
		if ok {
			counts[strings.Join(f[:len(f)-4], " ")] = n
		}
	}
	require.NotEmpty(t, counts, "no counts in:\n%s", out)
	return counts
}

// TestPostgresImportEndToEnd moves a real standalone server (SQLite, auth files, passphrase vault, a connected
// client with a command run) to a Postgres node with "rportd import", and checks the node serves the same data: the
// admin logs in with the old password, the client connects with its old credentials, the command history and the
// vault value (re-encrypted for the local key provider) are there. A second import adds nothing.
func TestPostgresImportEndToEnd(t *testing.T) {
	d := pgtest.NewEmpty(t) // skips without RPORT_TEST_POSTGRES_URL
	root := t.TempDir()

	// --- the standalone server
	srcRoot := filepath.Join(root, "standalone")
	srcData := filepath.Join(srcRoot, "data")
	require.NoError(t, os.MkdirAll(srcData, 0o700))
	hash, err := bcrypt.GenerateFromPassword([]byte(e2eAdminPass), bcrypt.MinCost)
	require.NoError(t, err)
	apiAuth := filepath.Join(srcRoot, "api-auth.json")
	require.NoError(t, os.WriteFile(apiAuth, []byte(fmt.Sprintf(`[{"username": %q, "password": %q, "groups": ["Administrators"]}]`,
		e2eAdmin, "$2y$"+strings.TrimPrefix(string(hash), "$2a$"))), 0o600))
	clientAuth := filepath.Join(srcRoot, "client-auth.json")
	require.NoError(t, os.WriteFile(clientAuth, []byte(`{"src-auth": "src-client-password-0"}`), 0o600))
	srcConf, srcClientPort, srcAPI := e2eConfig(t, srcRoot, srcData,
		fmt.Sprintf("  auth_file = %q", clientAuth), fmt.Sprintf("  auth_file = %q", apiAuth), "")

	src := startE2EServer(t, srcRoot, srcConf, srcClientPort, srcAPI, e2eAdmin, e2eAdminPass)
	src.connectClient("src-client", "src-auth", "src-client-password-0")
	var started struct {
		JID string `json:"jid"`
	}
	src.mustAPI(http.MethodPost, "/clients/src-client/commands", map[string]any{"command": "echo before-import", "timeout_sec": 10}, &started)
	require.Equal(t, "successful", src.waitJob("src-client", started.JID)["status"])
	const passphrase = "source-vault-passphrase-0"
	src.mustAPI(http.MethodPost, "/vault-admin/init", map[string]string{"password": passphrase}, nil)
	var stored struct {
		ID int `json:"id"`
	}
	src.mustAPI(http.MethodPost, "/vault", map[string]any{"client_id": "src-client", "key": "api-key", "value": "moved-secret", "type": "secret"}, &stored)
	src.stop()

	// --- the Postgres node
	nodeRoot := filepath.Join(root, "node")
	nodeData := filepath.Join(nodeRoot, "data")
	require.NoError(t, os.MkdirAll(nodeData, 0o700))
	keyFile := filepath.Join(nodeRoot, "vault.key")
	require.NoError(t, os.WriteFile(keyFile, []byte(hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))), 0o600))
	nodeConf, nodeClientPort, nodeAPI := e2eConfig(t, nodeRoot, nodeData, "", "", fmt.Sprintf(`
[storage]
  backend = "postgres"
  dsn = %q

[vault]
  key_provider = "local"
  local_key_file = %q
`, d.AppURL, keyFile))
	runRportd(t, "", "migrate", "-c", nodeConf, "--dsn", d.OwnerURL, "--grant-to", pgtest.AppRole)

	importArgs := []string{"import", "-c", nodeConf, "--from", srcData, "--from-config", srcConf, "--with-history", "--dsn", d.OwnerURL}
	dry := runRportdEnv(t, []string{EnvVaultPassphrase + "=" + passphrase}, append(importArgs, "--dry-run")...)
	assert.Contains(t, dry, "dry run: nothing was written")
	first := runRportdEnv(t, []string{EnvVaultPassphrase + "=" + passphrase}, importArgs...)
	t.Logf("rportd import:\n%s", first)
	c := importCounts(t, first)
	for _, e := range []string{"users", "client auth", "clients", "vault values", "jobs"} {
		assert.Positive(t, c[e][1], "%s added", e)
	}
	again := runRportdEnv(t, []string{EnvVaultPassphrase + "=" + passphrase}, importArgs...)
	for e, n := range importCounts(t, again) {
		assert.Zero(t, n[1], "second import adds no %s:\n%s", e, again)
	}

	// without the passphrase the vault can't be moved to the managed key, and nothing is written
	rportd, _ := e2eBinaries(t)
	noPass := exec.Command(rportd, importArgs...)
	noPass.Env = os.Environ()
	out, err := noPass.CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(out), "passphrase")

	node := startE2EServer(t, nodeRoot, nodeConf, nodeClientPort, nodeAPI, e2eAdmin, e2eAdminPass)
	node.connectClient("src-client", "src-auth", "src-client-password-0")
	var jobs []map[string]any
	node.mustAPI(http.MethodGet, "/clients/src-client/commands", nil, &jobs)
	require.NotEmpty(t, jobs)
	assert.Equal(t, started.JID, jobs[len(jobs)-1]["jid"])
	var value map[string]any
	node.mustAPI(http.MethodGet, fmt.Sprintf("/vault/%d", stored.ID), nil, &value)
	assert.Equal(t, "moved-secret", value["value"])
	t.Logf("after import: admin logged in, client reconnected, %d job(s), vault value %d read back", len(jobs), stored.ID)
}

// runRportdEnv is runRportd with extra environment variables.
func runRportdEnv(t *testing.T, env []string, args ...string) string {
	t.Helper()
	rportd, _ := e2eBinaries(t)
	cmd := exec.Command(rportd, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "rportd %s:\n%s", strings.Join(args, " "), out)
	return string(out)
}
