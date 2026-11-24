//go:build !unix

package importer

import "os"

// lockHolder can't ask for POSIX locks here; it only checks that path exists. The process check and the SQLite side
// files remain.
func lockHolder(path string) (int, error) {
	_, err := os.Stat(path)
	return 0, err
}

const lockCheckSupported = false
