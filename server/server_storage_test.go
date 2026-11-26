package chserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/clients"
	"github.com/pohcee/procy-server/server/clients/clientdata"
	"github.com/pohcee/procy-server/share/files"
	"github.com/pohcee/procy-server/share/logger"
)

// failingClients makes NewServer fail after the storage is open: loading the initial clients fails.
type failingClients struct {
	clients.ClientStore
}

func (failingClients) GetAll(context.Context, *logger.Logger) ([]*clientdata.Client, error) {
	return nil, errors.New("clients unavailable")
}

// openFiles returns how many file descriptors of this process are open on path.
func openFiles(t *testing.T, path string) int {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	require.NoError(t, err)
	n := 0
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		if err == nil && target == path {
			n++
		}
	}
	return n
}

func TestNewServerClosesStorageWhenStartupFails(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("counts open files through /proc/self/fd")
	}
	config := storageTestConfig(t)
	config.API.AuditLog.Enable = true
	// clients.db is still opened: StoredTunnels is not given
	opts := &ServerOpts{FilesAPI: files.NewFileSystem(), Repositories: &Repositories{Clients: failingClients{}}}

	_, err := NewServer(context.Background(), config, opts)

	require.ErrorContains(t, err, "clients unavailable")
	for _, name := range []string{"clients.db", "jobs.db", "api_sessions.db", "auditlog.db"} {
		path := filepath.Join(config.Server.DataDir, name)
		require.FileExists(t, path, "NewServer opened it")
		assert.Zero(t, openFiles(t, path), "%s is still open", name)
	}
}
