//go:build windows
// +build windows

package chserver

import "github.com/pohcee/procy-server/server/chconfig"

// Contains constants applicable only to windows OS.
const (
	DefaultDataDirectory = chconfig.DefaultDataDirectory
)
