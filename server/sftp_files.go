package chserver

import (
	"io"
	"os"
	"path/filepath"

	"github.com/pkg/sftp"
)

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
