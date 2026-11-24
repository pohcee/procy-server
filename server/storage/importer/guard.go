package importer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// guard refuses a data dir a running server holds: a process with a file open in it (where /proc shows it) or a
// POSIX lock on one of its SQLite files or their -shm files (SQLite holds one for as long as a connection is open,
// whoever owns the process). extra are further SQLite files to check (the [database] file of the auth tables).
func guard(dataDir string, extra []string) (warnings []string, err error) {
	dirs := []string{dataDir}
	for _, f := range extra {
		if d := filepath.Dir(f); d != dataDir {
			dirs = append(dirs, d)
		}
	}
	for _, d := range dirs {
		if holders := processesHolding(d); len(holders) > 0 {
			return nil, fmt.Errorf("%s is in use, stop rportd first: %s", d, strings.Join(holders, "; "))
		}
	}
	if err := checkLocks(dataDir, extra); err != nil {
		return nil, err
	}
	if !lockCheckSupported {
		warnings = append(warnings, "SQLite locks can't be checked on "+runtime.GOOS+": make sure rportd is stopped")
	}
	side, err := sqliteSideFiles(dataDir)
	if err != nil {
		return nil, err
	}
	if len(side) > 0 {
		sort.Strings(side)
		// normal after a stop: nothing holds them, so the WAL is complete; it is replayed into a copy
		warnings = append(warnings, fmt.Sprintf("SQLite side files present with no process holding them (%s): those databases were read from a copy with the WAL replayed", strings.Join(side, ", ")))
	}
	return warnings, nil
}

// checkLocks refuses SQLite files (those of the data dir and extra) on which another process holds a lock.
func checkLocks(dataDir string, extra []string) error {
	files, err := filepath.Glob(filepath.Join(dataDir, "*.db"))
	if err != nil {
		return err
	}
	files = append(files, extra...)
	for _, db := range files {
		for _, f := range []string{db, db + "-shm"} {
			pid, err := lockHolder(f)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("cannot check whether a process holds %s (%v); run as the rportd user or root", f, err)
			}
			if pid != 0 {
				return fmt.Errorf("%s is locked by pid %d: rportd is running, stop it first", f, pid)
			}
		}
	}
	return nil
}

// sqliteSideFiles returns the -wal, -shm and -journal files in dir.
func sqliteSideFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, "-wal") || strings.HasSuffix(n, "-shm") || strings.HasSuffix(n, "-journal") {
			out = append(out, n)
		}
	}
	return out, nil
}

// processesHolding lists other processes with a file open under dir (Linux /proc only). Processes of other users
// are invisible without privileges; the lock check covers those.
func processesHolding(dir string) []string {
	if runtime.GOOS != "linux" {
		return nil
	}
	abs, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil
	}
	prefix := abs + string(filepath.Separator)
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	self := fmt.Sprint(os.Getpid())
	var out []string
	for _, p := range procs {
		pid := p.Name()
		if pid == self || pid[0] < '0' || pid[0] > '9' {
			continue
		}
		fds, err := os.ReadDir(filepath.Join("/proc", pid, "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join("/proc", pid, "fd", fd.Name()))
			if err != nil {
				continue
			}
			if target == abs || strings.HasPrefix(target, prefix) {
				comm, _ := os.ReadFile(filepath.Join("/proc", pid, "comm"))
				out = append(out, fmt.Sprintf("pid %s (%s) has %s open", pid, strings.TrimSpace(string(comm)), target))
				break
			}
		}
	}
	return out
}
