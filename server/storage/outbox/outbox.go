// Package outbox is the local outbox of a device process: what devices report (job results, measurements,
// connection events, audit entries, client state) is written to a SQLite file first and shipped to the database
// in batches by a Shipper, so a device process loses nothing while the database is unreachable and pays the
// database round trip once per batch. Items survive restarts. Each tenant's items are capped: when a tenant is
// full the oldest measurement is dropped first, then the oldest connection event, audit entry or client state;
// job results are never dropped.
package outbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3" // registers the sqlite3 driver

	"github.com/openrport/openrport/share/tenant"
)

// Kind is what an item is; each kind is shipped through its own ingest function.
type Kind string

// The kinds of items, in the order a Shipper ships them.
const (
	KindJobResult       Kind = "job_result"
	KindClientState     Kind = "client_state"
	KindConnectionEvent Kind = "connection_event"
	KindAudit           Kind = "audit"
	KindMeasurement     Kind = "measurement"
)

// Kinds are all kinds in shipping order.
var Kinds = []Kind{KindJobResult, KindClientState, KindConnectionEvent, KindAudit, KindMeasurement}

// dropOrder is the order in which a full tenant's oldest items are dropped. Job results are never dropped.
var dropOrder = []Kind{KindMeasurement, KindConnectionEvent, KindAudit, KindClientState}

// DefaultMaxPerTenant is the cap of a tenant's items when Options.MaxPerTenant is 0.
const DefaultMaxPerTenant = 100_000

// Item is one entry of the outbox.
type Item struct {
	ID     int64     `db:"id"`
	Kind   Kind      `db:"kind"`
	Tenant tenant.ID `db:"tenant_id"`
	// Key is the item's idempotency key, unique per kind and tenant.
	Key string `db:"key"`
	// Payload is the item as its ingest function takes it (a JSON object).
	Payload []byte    `db:"payload"`
	Added   time.Time `db:"added_at"`
}

// Options configure an Outbox.
type Options struct {
	// MaxPerTenant caps the items each tenant may hold (DefaultMaxPerTenant when 0).
	MaxPerTenant int
}

// Outbox is the outbox file. It is safe for concurrent use.
type Outbox struct {
	db      *sqlx.DB
	max     int
	mu      sync.Mutex // serializes Add, so the cap holds
	wake    chan struct{}
	dropped atomic.Int64
}

const schema = `CREATE TABLE IF NOT EXISTS outbox (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	kind      TEXT    NOT NULL,
	tenant_id TEXT    NOT NULL,
	key       TEXT    NOT NULL,
	payload   BLOB    NOT NULL,
	added_at  DATETIME NOT NULL,
	UNIQUE (kind, tenant_id, key)
);
CREATE INDEX IF NOT EXISTS outbox_kind ON outbox (kind, id);
CREATE INDEX IF NOT EXISTS outbox_tenant_kind ON outbox (tenant_id, kind, id);`

// Open opens (creating it when missing) the outbox file at path.
func Open(path string, opts Options) (*Outbox, error) {
	// synchronous FULL: an item Add returned for survives a power loss
	db, err := sqlx.Open("sqlite3", "file:"+path+"?_journal_mode=WAL&_sync=FULL&_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("outbox: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("outbox: open %s: %w", path, err)
	}
	if opts.MaxPerTenant <= 0 {
		opts.MaxPerTenant = DefaultMaxPerTenant
	}
	return &Outbox{db: db, max: opts.MaxPerTenant, wake: make(chan struct{}, 1)}, nil
}

// Close closes the file.
func (o *Outbox) Close() error { return o.db.Close() }

// Wake is signalled after an item was added.
func (o *Outbox) Wake() <-chan struct{} { return o.wake }

// Dropped is the number of items dropped because their tenant was full, since Open.
func (o *Outbox) Dropped() int64 { return o.dropped.Load() }

// Add stores an item. An item with the kind, tenant and key of a stored one is ignored. When the tenant holds
// MaxPerTenant items the oldest droppable item is dropped to make room; when only job results are left, a new job
// result is stored anyway and any other new item is dropped.
func (o *Outbox) Add(ctx context.Context, kind Kind, tid tenant.ID, key string, payload []byte) error {
	if tid == "" {
		return fmt.Errorf("outbox: %w", tenant.ErrMissing)
	}
	if key == "" {
		return errors.New("outbox: empty key")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	tx, err := o.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("outbox: add: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var n int
	if err := tx.GetContext(ctx, &n, "SELECT count(*) FROM outbox WHERE tenant_id = ?", tid); err != nil {
		return fmt.Errorf("outbox: add: %w", err)
	}
	if n >= o.max {
		made, err := dropOldest(ctx, tx, tid)
		if err != nil {
			return fmt.Errorf("outbox: add: %w", err)
		}
		if !made && kind != KindJobResult {
			o.dropped.Add(1)
			return nil
		}
		if made {
			o.dropped.Add(1)
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO outbox (kind, tenant_id, key, payload, added_at) VALUES (?, ?, ?, ?, ?)",
		kind, tid, key, payload, time.Now().UTC()); err != nil {
		return fmt.Errorf("outbox: add: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("outbox: add: %w", err)
	}
	select {
	case o.wake <- struct{}{}:
	default:
	}
	return nil
}

func dropOldest(ctx context.Context, tx *sqlx.Tx, tid tenant.ID) (bool, error) {
	for _, k := range dropOrder {
		res, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE id = (
			SELECT id FROM outbox WHERE tenant_id = ? AND kind = ? ORDER BY id LIMIT 1)`, tid, k)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return true, nil
		}
	}
	return false, nil
}

// Pending returns up to limit of the oldest items of kind.
func (o *Outbox) Pending(ctx context.Context, kind Kind, limit int) ([]Item, error) {
	var items []Item
	if err := o.db.SelectContext(ctx, &items, "SELECT id, kind, tenant_id, key, payload, added_at FROM outbox WHERE kind = ? ORDER BY id LIMIT ?", kind, limit); err != nil {
		return nil, fmt.Errorf("outbox: pending: %w", err)
	}
	return items, nil
}

// Remove deletes the items with ids (shipped ones).
func (o *Outbox) Remove(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	q, args, err := sqlx.In("DELETE FROM outbox WHERE id IN (?)", ids)
	if err != nil {
		return err
	}
	if _, err := o.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("outbox: remove: %w", err)
	}
	return nil
}

// Count returns how many items the outbox holds for tid, or for every tenant when tid is empty.
func (o *Outbox) Count(ctx context.Context, tid tenant.ID) (int, error) {
	var n int
	var err error
	if tid == "" {
		err = o.db.GetContext(ctx, &n, "SELECT count(*) FROM outbox")
	} else {
		err = o.db.GetContext(ctx, &n, "SELECT count(*) FROM outbox WHERE tenant_id = ?", tid)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}
