//go:build cgo

package sqlite

import sqlite3drv "github.com/mattn/go-sqlite3"

// isBusy reports whether err is SQLite's "database is busy".
func isBusy(err error) bool {
	sqlErr, ok := err.(sqlite3drv.Error)
	return ok && sqlErr.Code == sqlite3drv.ErrBusy
}
