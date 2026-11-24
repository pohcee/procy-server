package chserver

import (
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/pkg/sftp"
)

// fileGrants records which server files each client connection may read over SFTP.
// A file is granted only while it is being sent to that client (see sendFileToClient).
type fileGrants struct {
	mu     sync.Mutex
	byConn map[any]map[string]int
}

func newFileGrants() *fileGrants {
	return &fileGrants{byConn: map[any]map[string]int{}}
}

func cleanGrantPath(p string) string {
	return path.Clean(filepath.ToSlash(p))
}

// Grant lets conn read p until the returned function is called. Grants of the same file are counted.
func (g *fileGrants) Grant(conn any, p string) (revoke func()) {
	p = cleanGrantPath(p)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byConn[conn] == nil {
		g.byConn[conn] = map[string]int{}
	}
	g.byConn[conn][p]++

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			files := g.byConn[conn]
			files[p]--
			if files[p] <= 0 {
				delete(files, p)
			}
			if len(files) == 0 {
				delete(g.byConn, conn)
			}
		})
	}
}

// Allowed reports whether conn may read p.
func (g *fileGrants) Allowed(conn any, p string) bool {
	p = cleanGrantPath(p)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.byConn[conn][p] > 0
}

// newDeviceFileServer returns an SFTP server for a client connection that can only read
// the files allowed reports true for. Listing, writing and every other command are refused.
func newDeviceFileServer(rwc io.ReadWriteCloser, allowed func(path string) bool) *sftp.RequestServer {
	h := &deviceFileHandler{allowed: allowed}
	return sftp.NewRequestServer(rwc, sftp.Handlers{
		FileGet:  h,
		FilePut:  h,
		FileCmd:  h,
		FileList: h,
	})
}

type deviceFileHandler struct {
	allowed func(path string) bool
}

func (h *deviceFileHandler) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	if !h.allowed(r.Filepath) {
		return nil, sftp.ErrSSHFxPermissionDenied
	}
	f, err := os.Open(filepath.FromSlash(r.Filepath))
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (h *deviceFileHandler) Filewrite(*sftp.Request) (io.WriterAt, error) {
	return nil, sftp.ErrSSHFxPermissionDenied
}

func (h *deviceFileHandler) Filecmd(*sftp.Request) error {
	return sftp.ErrSSHFxPermissionDenied
}

func (h *deviceFileHandler) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	if r.Method != "Stat" || !h.allowed(r.Filepath) {
		return nil, sftp.ErrSSHFxPermissionDenied
	}
	fi, err := os.Stat(filepath.FromSlash(r.Filepath))
	if err != nil {
		return nil, err
	}
	return statLister{fi}, nil
}

type statLister []os.FileInfo

func (l statLister) ListAt(ls []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(ls, l[offset:])
	if n < len(ls) {
		return n, io.EOF
	}
	return n, nil
}
