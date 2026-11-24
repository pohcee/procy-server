//go:build unix

package importer

import (
	"os"
	"syscall"
)

// lockHolder returns the pid of a process holding a POSIX lock on path, or 0. SQLite takes fcntl locks: a WAL
// connection keeps a shared lock on the -shm file while it is open, and transactions lock the database file. F_GETLK
// only asks; it works on a read-only descriptor and sees the locks of other users' processes (not this process's
// own).
func lockHolder(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lk); err != nil {
		return 0, err
	}
	if lk.Type == syscall.F_UNLCK {
		return 0, nil
	}
	return int(lk.Pid), nil
}

const lockCheckSupported = true
