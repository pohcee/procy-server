package chserver

import (
	"context"
	"fmt"
	"time"

	"github.com/openrport/openrport/server/library"
	chshare "github.com/openrport/openrport/share"
	"github.com/openrport/openrport/share/logger"
)

// openLibrary returns the library manager on store, after importing the scripts and commands of
// old versions (when store supports it) and syncing the built-in entries. ctx carries the tenant.
func openLibrary(ctx context.Context, store library.Repository, log *logger.Logger) (*library.Manager, error) {
	if importer, ok := store.(library.LegacyImporter); ok {
		imported, err := importer.ImportLegacy(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to import scripts and commands into the library: %w", err)
		}
		if imported > 0 {
			log.Infof("imported %d scripts and commands into the library", imported)
		}
	}

	// Built-ins are a convenience: a failure here must not stop rportd.
	if builtins, err := library.Builtins(); err != nil {
		log.Errorf("library: failed to load built-in entries: %v", err)
	} else {
		created, updated, skipped, err := library.SyncBuiltins(ctx, store, builtins, chshare.BuildVersion, time.Now())
		if err != nil {
			log.Errorf("library: failed to sync built-in entries: %v", err)
		}
		if created > 0 || updated > 0 {
			log.Infof("library: added %d, updated %d built-in entries", created, updated)
		}
		for _, key := range skipped {
			log.Infof("library: built-in %s was deleted; not restoring it", key)
		}
	}

	return library.NewManager(store, time.Now), nil
}
