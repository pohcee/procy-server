package pairing

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setClientServers runs set_client_servers from functions.sh on a config with the given content.
func setClientServers(t *testing.T, conf string, args ...string) (string, error) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "rport.conf")
	require.NoError(t, os.WriteFile(path, []byte(conf), 0640))
	fns := filepath.Join(dir, "functions.sh")
	require.NoError(t, os.WriteFile(fns, mustRead("templates/linux/functions.sh"), 0600))
	cmd := exec.Command(sh, append([]string{"-c", `. "$0"; set_client_servers "$@"`, fns, path}, args...)...)
	out, runErr := cmd.CombinedOutput()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	if runErr != nil {
		t.Logf("output: %s", out)
	}
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0640), info.Mode().Perm(), "the file mode is kept")
	return string(data), runErr
}

const newServer = "http://connect.procy.sh"
const fallback = "http://us-central1.connect.procy.sh:80"

func TestSetClientServersReplacesServerOnlyInClientSection(t *testing.T) {
	got, err := setClientServers(t, "[client]\n  server = \"old.example.com:80\"\n  auth = \"a:b\"\n\n[connection]\n  server = \"keep\"\n", newServer, fallback)
	require.NoError(t, err)
	assert.Equal(t, "[client]\n  server = \""+newServer+"\"\n  fallback_servers = [\""+fallback+"\"]\n  auth = \"a:b\"\n\n[connection]\n  server = \"keep\"\n", got)
}

func TestSetClientServersReplacesExistingFallback(t *testing.T) {
	got, err := setClientServers(t, "[client]\n  fallback_servers = ['x']\n  server = 'old'\n", newServer, fallback)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(got, "fallback_servers"), got)
	assert.NotContains(t, got, "'x'")
	assert.Contains(t, got, "fallback_servers = [\""+fallback+"\"]")
}

func TestSetClientServersReplacesMultilineFallback(t *testing.T) {
	got, err := setClientServers(t, "[client]\n  server = 'old'\n  fallback_servers = [\n    'x',\n    'y'\n  ]\n  auth = 'a:b'\n", newServer, fallback)
	require.NoError(t, err)
	assert.Equal(t, "[client]\n  server = \""+newServer+"\"\n  fallback_servers = [\""+fallback+"\"]\n  auth = 'a:b'\n", got)
}

func TestSetClientServersKeepsComments(t *testing.T) {
	got, err := setClientServers(t, "[client]\n  #server = \"example\"\n  #fallback_servers = ['a']\n  server = 'old'\n", newServer)
	require.NoError(t, err)
	assert.Contains(t, got, "  #server = \"example\"\n  #fallback_servers = ['a']\n")
	assert.Contains(t, got, "  server = \""+newServer+"\"\n  fallback_servers = []\n")
}

func TestSetClientServersFailsWithoutServer(t *testing.T) {
	for _, conf := range []string{"[client]\n  auth = 'a:b'\n[connection]\n  server = 'x'\n", "server = 'x'\n"} {
		got, err := setClientServers(t, conf, newServer, fallback)
		assert.Error(t, err)
		assert.Equal(t, conf, got, "the config is unchanged")
	}
}

func TestSetClientServersKeepsCRLF(t *testing.T) {
	got, err := setClientServers(t, "[client]\r\n  server = 'old'\r\n  auth = 'a:b'\r\n", newServer, fallback)
	require.NoError(t, err)
	assert.Contains(t, got, "  auth = 'a:b'\r\n")
	assert.Contains(t, got, "  server = \""+newServer+"\"")
	assert.NotContains(t, got, "'old'")
}

// The config holds the client credentials, so the rewritten copy must never be readable by others,
// even for a moment. A shim awk records the mode of the file its output goes to.
func TestSetClientServersWritesPrivateTempFile(t *testing.T) {
	realAwk, err := exec.LookPath("awk")
	if err != nil {
		t.Skip("no awk")
	}
	if _, err := os.Stat("/proc/self/fd/1"); err != nil {
		t.Skip("no /proc")
	}
	shimDir := t.TempDir()
	modeFile := filepath.Join(shimDir, "mode")
	shim := "#!/bin/sh\nstat -L -c %a /proc/$$/fd/1 > " + modeFile + "\nexec " + realAwk + " \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(shimDir, "awk"), []byte(shim), 0755))
	t.Setenv("PATH", shimDir+":"+os.Getenv("PATH"))
	_, err = setClientServers(t, "[client]\n  server = 'old'\n", newServer, fallback)
	require.NoError(t, err)
	mode, err := os.ReadFile(modeFile)
	require.NoError(t, err)
	assert.Equal(t, "600", strings.TrimSpace(string(mode)))
}
