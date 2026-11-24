//go:build !unix

package pgtest

// lockFile is a no-op where flock is not available: start the first Postgres test package alone to fill the cache.
func lockFile(string) (func(), error) { return func() {}, nil }
