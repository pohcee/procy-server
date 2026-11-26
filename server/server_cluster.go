package chserver

import (
	"context"
	"strconv"
	"time"

	"github.com/pohcee/procy-server/server/api/jobs/schedule"
	"github.com/pohcee/procy-server/server/api/session"
	"github.com/pohcee/procy-server/server/clients"
	"github.com/pohcee/procy-server/server/cluster"
	notificationsSQLite "github.com/pohcee/procy-server/server/notifications/repository/sqlite"
	"github.com/pohcee/procy-server/server/vault"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/tenant"
)

// newScheduleManager returns the schedule manager. Standalone it holds the schedules of ctx's tenant, as always. In
// a cluster it holds the schedules of every active tenant, every node runs them, and each run is claimed first so it
// starts on one node only.
func newScheduleManager(ctx context.Context, c cluster.Coordinator, log *logger.Logger, p schedule.Provider, runner schedule.JobRunner, timeoutSec int) (*schedule.Manager, error) {
	if !c.Clustered() {
		return schedule.New(ctx, log, p, runner, timeoutSec)
	}
	m := schedule.NewManager(ctx, runner, p, log, timeoutSec)
	m.SetClaimer(c)
	tenants, err := c.Tenants(ctx)
	if err != nil {
		m.Stop()
		return nil, err
	}
	for _, tid := range tenants {
		if err := m.Load(tenant.WithID(ctx, tid)); err != nil {
			m.Stop()
			return nil, err
		}
	}
	return m, nil
}

// runClusterNotificationsCleanup deletes old notifications of every tenant on one node at a time. Standalone the
// API listener's cleaner does it.
func (s *Server) runClusterNotificationsCleanup(ctx context.Context) {
	if !s.cluster.Clustered() {
		return
	}
	cleanable, ok := s.repos.Notifications.(notificationsSQLite.Cleanable)
	if !ok {
		return
	}
	keep := s.config.Notifications.LogStorageDuration
	task := taskFunc(func(ctx context.Context) error {
		return cleanable.DeleteBefore(ctx, time.Now().Add(-keep).UTC())
	})
	go runPeriodic(ctx, s.cluster, s.Logger.Fork("notifications cleaner"), "notifications-cleanup", task, s.config.Notifications.CleanupInterval)
}

// cacheWatcher keeps what this node caches in step with the changes every node makes (the change feed).
type cacheWatcher struct {
	log       *logger.Logger
	cluster   cluster.Coordinator
	clients   *clients.ClientRepository
	sessions  *session.Cache
	schedules *schedule.Manager
	vaultKeys vault.KeyProvider
}

// keyForgetter is a vault key provider that caches keys (vaultkms.Provider).
type keyForgetter interface {
	Forget(tid tenant.ID)
	ForgetAll()
}

// watchCluster registers the server's caches with the cluster. Standalone it does nothing.
func (s *Server) watchCluster() {
	if !s.cluster.Clustered() {
		return
	}
	w := &cacheWatcher{
		log:       s.Logger.Fork("cluster"),
		cluster:   s.cluster,
		clients:   s.clientService.GetRepo(),
		schedules: s.scheduleManager,
		vaultKeys: s.vaultKeys,
	}
	if s.apiListener != nil {
		w.sessions = s.apiListener.apiSessions
	}
	s.cluster.Watch(w)
}

// ordered returns changes with each (tenant, entity, key) once (the watcher re-reads the database, so the last of
// several is as good as all), session revocations first.
func ordered(changes []cluster.Change) []cluster.Change {
	type key struct{ tenant, entity, key string }
	seen := make(map[key]bool, len(changes))
	sessions := []cluster.Change{}
	rest := make([]cluster.Change, 0, len(changes))
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		k := key{string(c.Tenant), c.Entity, c.Key}
		if seen[k] {
			continue
		}
		seen[k] = true
		if c.Entity == cluster.EntityAPISessions {
			sessions = append(sessions, c)
		} else {
			rest = append(rest, c)
		}
	}
	reverse(sessions)
	reverse(rest)
	return append(sessions, rest...)
}

func reverse(cs []cluster.Change) {
	for i, j := 0, len(cs)-1; i < j; i, j = i+1, j-1 {
		cs[i], cs[j] = cs[j], cs[i]
	}
}

func (w *cacheWatcher) OnChanges(ctx context.Context, changes []cluster.Change) {
	for _, c := range ordered(changes) {
		tctx := tenant.WithID(ctx, c.Tenant)
		var err error
		switch c.Entity {
		case cluster.EntityClients:
			if w.clients != nil {
				err = w.clients.Refresh(tctx, c.Key)
			}
		case cluster.EntityAPISessions:
			if c.Op != cluster.OpDelete || w.sessions == nil {
				continue
			}
			id, perr := strconv.ParseInt(c.Key, 10, 64)
			if perr != nil {
				err = perr
				break
			}
			w.sessions.Forget(c.Tenant, id)
		case cluster.EntitySchedules:
			if w.schedules != nil {
				err = w.schedules.Reload(tctx, c.Key)
			}
		case cluster.EntityVaultWrappedKeys:
			if f, ok := w.vaultKeys.(keyForgetter); ok {
				f.Forget(c.Tenant)
			}
		}
		if err != nil {
			w.log.Errorf("change of %s %q in tenant %s: %v", c.Entity, c.Key, c.Tenant, err)
		}
	}
}

func (w *cacheWatcher) OnReset(ctx context.Context) {
	if w.clients != nil {
		if err := w.clients.Reload(ctx); err != nil {
			w.log.Errorf("reload clients: %v", err)
		}
	}
	if w.sessions != nil {
		w.sessions.ForgetAll()
	}
	if f, ok := w.vaultKeys.(keyForgetter); ok {
		f.ForgetAll()
	}
	if w.schedules == nil {
		return
	}
	tenants, err := w.cluster.Tenants(ctx)
	if err != nil {
		w.log.Errorf("reload schedules: %v", err)
		return
	}
	for _, tid := range tenants {
		// schedules deleted meanwhile stay in the cron, but their runs find nothing and do nothing
		if err := w.schedules.Load(tenant.WithID(ctx, tid)); err != nil {
			w.log.Errorf("reload schedules of tenant %s: %v", tid, err)
		}
	}
}
