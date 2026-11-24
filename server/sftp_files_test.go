package chserver

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/dispatch"
	"github.com/openrport/openrport/share/test"
)

// startSFTP serves the device file server over an in-memory pipe and returns a client connected to it.
func startSFTP(t *testing.T, allowed func(path string) bool) *sftp.Client {
	t.Helper()
	serverSide, clientSide := net.Pipe()
	srv := newDeviceFileServer(serverSide, allowed)
	go func() { _ = srv.Serve() }()
	cl, err := sftp.NewClientPipe(clientSide, clientSide)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = cl.Close()
		_ = srv.Close()
	})
	return cl
}

func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0600))
	return p
}

func TestDeviceFileServerReadsGrantedFile(t *testing.T) {
	dir := t.TempDir()
	granted := writeTemp(t, dir, "abc_rport_filepush", "payload")
	cl := startSFTP(t, func(p string) bool { return p == granted })

	f, err := cl.Open(granted)
	require.NoError(t, err)
	data, err := io.ReadAll(f)
	require.NoError(t, err)
	assert.Equal(t, "payload", string(data))
	require.NoError(t, f.Close())
}

func TestDeviceFileServerRefusesOtherFiles(t *testing.T) {
	dir := t.TempDir()
	granted := writeTemp(t, dir, "abc_rport_filepush", "payload")
	secret := writeTemp(t, dir, "rportd.conf", "jwt_secret = x")
	cl := startSFTP(t, func(p string) bool { return p == granted })

	_, err := cl.Open(secret)
	assert.ErrorIs(t, err, os.ErrPermission, "open of a file that isn't being sent")

	_, err = cl.Open(dir + "/sub/../rportd.conf")
	assert.ErrorIs(t, err, os.ErrPermission, "path tricks")

	_, err = cl.Stat(secret)
	assert.ErrorIs(t, err, os.ErrPermission, "stat")

	_, err = cl.ReadDir(dir)
	assert.ErrorIs(t, err, os.ErrPermission, "list")

	_, err = cl.Create(filepath.Join(dir, "new"))
	assert.ErrorIs(t, err, os.ErrPermission, "create")

	err = cl.Remove(granted)
	assert.ErrorIs(t, err, os.ErrPermission, "remove of the granted file")

	_, err = cl.OpenFile(granted, os.O_WRONLY)
	assert.ErrorIs(t, err, os.ErrPermission, "write to the granted file")
}

func TestDeviceFileServerStatsGrantedFile(t *testing.T) {
	dir := t.TempDir()
	granted := writeTemp(t, dir, "abc_rport_filepush", "payload")
	cl := startSFTP(t, func(p string) bool { return p == granted })

	fi, err := cl.Stat(granted)
	require.NoError(t, err)
	assert.EqualValues(t, 7, fi.Size())
}

func TestClientListenerSessionChannelServesOnlyGrantedFiles(t *testing.T) {
	dir := t.TempDir()
	granted := writeTemp(t, dir, "abc_rport_filepush", "payload")
	secret := writeTemp(t, dir, "rportd.conf", "jwt_secret = x")
	grants := dispatch.NewFileGrants()
	conn := test.NewConnMock()
	defer grants.Grant(conn, granted)()
	cl := &ClientListener{server: &Server{fileGrants: grants}}

	serverSide, clientSide := net.Pipe()
	go cl.handleSessionChannel(conn, serverSide, testLog)
	sc, err := sftp.NewClientPipe(clientSide, clientSide)
	require.NoError(t, err)
	defer sc.Close()

	f, err := sc.Open(granted)
	require.NoError(t, err)
	data, err := io.ReadAll(f)
	require.NoError(t, err)
	assert.Equal(t, "payload", string(data))

	_, err = sc.Open(secret)
	assert.ErrorIs(t, err, os.ErrPermission)
}
