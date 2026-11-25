package chserver

import (
	"context"
	"fmt"
	"time"

	"github.com/openrport/openrport/server/cluster"
	"github.com/openrport/openrport/server/library"
	chshare "github.com/openrport/openrport/share"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

// builtinsLeaseTTL keeps other nodes of the same version from syncing the built-ins again while they start.
const builtinsLeaseTTL = 10 * time.Minute

// openLibrary returns the library manager on store, after importing the scripts and commands of
// old versions (when store supports it) and syncing the built-in entries. ctx carries the tenant. In a cluster the
// built-ins are synced for every active tenant, by one node of each version (a lease per tenant and version), and
// nothing is imported (a cluster has no old data dir); the tenants synced are returned, so followBuiltins can sync
// the ones that become active later. A process that runs no API work (device process) syncs nothing.
func openLibrary(ctx context.Context, store library.Repository, log *logger.Logger, c cluster.Coordinator, apiWork bool) (*library.Manager, map[tenant.ID]bool, error) {
	if c.Clustered() {
		var synced map[tenant.ID]bool
		if apiWork {
			synced = runForTenants(ctx, c, log, builtinsLease(), builtinsLeaseTTL, builtinsTask(store, log), nil)
		}
		return library.NewManager(store, time.Now), synced, nil
	}
	if importer, ok := store.(library.LegacyImporter); ok {
		imported, err := importer.ImportLegacy(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to import scripts and commands into the library: %w", err)
		}
		if imported > 0 {
			log.Infof("imported %d scripts and commands into the library", imported)
		}
	}

	syncBuiltins(ctx, store, log)
	return library.NewManager(store, time.Now), nil, nil
}

func builtinsLease() string { return leaseName("library-builtins", chshare.BuildVersion) }

func builtinsTask(store library.Repository, log *logger.Logger) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		syncBuiltins(ctx, store, log)
		return nil
	}
}

// followBuiltins syncs the built-in entries of the tenants that become active after openLibrary listed them (synced),
// until ctx is done. Organizations created through the operator API or rportd get them when they are created; this
// covers the others (and a version's built-ins for a tenant active again). Without synced (listing the tenants at
// start failed) every tenant is synced at the first refresh.
func followBuiltins(ctx context.Context, store library.Repository, log *logger.Logger, c cluster.Coordinator, synced map[tenant.ID]bool) {
	if !c.Clustered() {
		return
	}
	followNewTenants(ctx, c, log, builtinsLease(), builtinsLeaseTTL, builtinsTask(store, log), synced)
}

// syncBuiltins syncs the built-in entries of ctx's tenant. Built-ins are a convenience: a failure is logged and
// does not stop rportd.
func syncBuiltins(ctx context.Context, store library.Repository, log *logger.Logger) {
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
}
