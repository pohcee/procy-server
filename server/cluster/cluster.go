// Package cluster is what a server needs to know about the other nodes sharing its database: which tenants its
// background work runs for, leases that make each periodic task run on one node at a time, claims that make a
// scheduled run start once, and the changes other nodes made to what this node caches.
//
// A standalone server (SQLite) is a cluster of one: Standalone runs everything for the default tenant, always holds
// every lease, claims every run and never reports a change, so it behaves exactly as before clusters existed.
package cluster

import (
	"context"
	"time"

	"github.com/pohcee/procy-server/share/tenant"
)

// Ops of a Change.
const (
	OpInsert = "insert"
	OpUpdate = "update"
	OpDelete = "delete"
)

// Entities of a Change: the tracked tables of the Postgres change feed.
const (
	EntityUsers            = "users"
	EntityUserGroups       = "user_groups"
	EntityGroups           = "groups"
	EntityAPITokens        = "api_tokens"
	EntityAPISessions      = "api_sessions"
	EntityClientsAuth      = "clients_auth"
	EntityClients          = "clients"
	EntityStoredTunnels    = "stored_tunnels"
	EntityClientGroups     = "client_groups"
	EntitySchedules        = "schedules"
	EntityLibraryEntries   = "library_entries"
	EntityVaultWrappedKeys = "vault_wrapped_keys"
)

// Change is a row of Entity, keyed Key, inserted, updated or deleted (Op) in Tenant, by any node (this one too).
type Change struct {
	Tenant tenant.ID
	Entity string
	Key    string
	Op     string
}

// Watcher reacts to changes. OnChanges gets every change in the order of its transaction's id (not quite commit order: the rows of each transaction together, transactions ordered by when they first wrote), at least once; OnReset is called when changes may have
// been missed, so everything cached must be dropped or reloaded. Both run on the node's feed goroutine and must not
// block for long.
type Watcher interface {
	OnChanges(ctx context.Context, changes []Change)
	OnReset(ctx context.Context)
}

// Coordinator is the server's view of its cluster.
type Coordinator interface {
	// NodeID names this node (in leases and claims).
	NodeID() string
	// Tenants lists the tenants background work runs for (the active ones).
	Tenants(ctx context.Context) ([]tenant.ID, error)
	// Lease reports whether this node holds the lease name of tid for ttl from now: it takes a free or expired lease,
	// renews its own, and gets false while another node holds it. tid is "" for work that belongs to no tenant.
	Lease(ctx context.Context, name string, tid tenant.ID, ttl time.Duration) (bool, error)
	// ClaimRun reports whether this node is the one to start the run of schedule scheduleID (tenant from ctx) due at
	// fire: true exactly once per (tenant, schedule, fire time) across the cluster.
	ClaimRun(ctx context.Context, scheduleID string, fire time.Time) (bool, error)
	// Watch adds w to the watchers of changes. Watchers added before Run see every change committed after the
	// coordinator was made.
	Watch(w Watcher)
	// Run delivers changes to the watchers and does the coordinator's own upkeep until ctx is done.
	Run(ctx context.Context) error
	// Clustered reports whether other nodes may share the database (false for Standalone).
	Clustered() bool
}

// Standalone is the Coordinator of a server alone on its database.
type Standalone struct{}

var _ Coordinator = Standalone{}

func (Standalone) NodeID() string { return "" }

func (Standalone) Tenants(context.Context) ([]tenant.ID, error) {
	return []tenant.ID{tenant.Default}, nil
}

func (Standalone) Lease(context.Context, string, tenant.ID, time.Duration) (bool, error) {
	return true, nil
}

func (Standalone) ClaimRun(context.Context, string, time.Time) (bool, error) { return true, nil }

func (Standalone) Watch(Watcher) {}

func (Standalone) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (Standalone) Clustered() bool { return false }

// WatcherFuncs adapts two functions to a Watcher; either may be nil.
type WatcherFuncs struct {
	Changes func(ctx context.Context, changes []Change)
	Reset   func(ctx context.Context)
}

func (w WatcherFuncs) OnChanges(ctx context.Context, changes []Change) {
	if w.Changes != nil {
		w.Changes(ctx, changes)
	}
}

func (w WatcherFuncs) OnReset(ctx context.Context) {
	if w.Reset != nil {
		w.Reset(ctx)
	}
}
