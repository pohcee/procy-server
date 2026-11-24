//go:build !windows
// +build !windows

package chserver

import "github.com/openrport/openrport/server/chconfig"

// Contains constants applicable only to non windows OS.
const (
	DefaultDataDirectory = chconfig.DefaultDataDirectory
)
