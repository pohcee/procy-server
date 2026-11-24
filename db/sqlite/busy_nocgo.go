//go:build !cgo

package sqlite

// isBusy is always false without cgo: the SQLite driver needs cgo, so there is no SQLite error to retry. This lets
// packages that use this one compile (for example for Windows without a C cross compiler).
func isBusy(error) bool { return false }
