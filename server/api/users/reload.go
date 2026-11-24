//go:build !windows
// +build !windows

package users

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// reload reloads API users from file when SIGUSR1 is received, until ctx (the server context) is done.
func (fa *FileAdapter) reload(ctx context.Context) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR1)
	defer signal.Stop(signals)
	fa.reloadOn(ctx, signals)
}
