package chserver

import (
	"io"
	"os"
	"path/filepath"

	"github.com/pkg/sftp"
)

// newDeviceFileServer returns an SFTP server for a client connection that can only read
// the files allowed reports true for. Listing, writing and every other command are refused. uploaded, when set,
// counts the bytes the client reads.
func newDeviceFileServer(rwc io.ReadWriteCloser, allowed func(path string) bool, uploaded func(n int64)) *sftp.RequestServer {
	h := &deviceFileHandler{allowed: allowed, uploaded: uploaded}
	return sftp.NewRequestServer(rwc, sftp.Handlers{
		FileGet:  h,
		FilePut:  h,
		FileCmd:  h,
		FileList: h,
	})
}

type deviceFileHandler struct {
	allowed  func(path string) bool
	uploaded func(n int64)
}

func (h *deviceFileHandler) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	if !h.allowed(r.Filepath) {
		return nil, sftp.ErrSSHFxPermissionDenied
	}
	f, err := os.Open(filepath.FromSlash(r.Filepath))
	if err != nil {
		return nil, err
	}
	if h.uploaded != nil {
		return countingReaderAt{f, h.uploaded}, nil
	}
	return f, nil
}

// countingReaderAt counts the bytes read: what the device actually fetched, so a transfer that stops half way
// counts half. It keeps the file's Close (the SFTP server closes what it opened).
type countingReaderAt struct {
	*os.File
	count func(n int64)
}

func (c countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.File.ReadAt(p, off)
	if n > 0 {
		c.count(int64(n))
	}
	return n, err
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
