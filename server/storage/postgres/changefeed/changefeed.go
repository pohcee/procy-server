// Package changefeed reads the change feed of the Postgres storage (table changes, migration 0600): one row per
// changed row of the tables nodes cache, written by a trigger in the same transaction as the change.
//
// Feed rows get their seq before their transaction commits, so they become visible out of seq order. Read therefore
// consumes only rows whose transaction is older than the oldest transaction still running
// (tx_id < pg_snapshot_xmin(pg_current_snapshot())), in (tx_id, seq) order, and a Cursor is the (tx_id, seq) of the
// last row consumed. Every row not consumed yet then lies above the cursor: its transaction was still running (or
// had no id yet) when the cursor's rows were read, so its id is at least the xmin of that read. A seq alone is not a
// safe cursor: a transaction that took an id early but wrote its row late commits a row with a lower seq after rows
// with higher seqs were consumed.
package changefeed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/share/tenant"
)

// Ops of a change.
const (
	OpInsert = "insert"
	OpUpdate = "update"
	OpDelete = "delete"
)

// ErrTooFarBehind is returned by Read when retention removed rows after the cursor: the reader missed changes and
// must start again from a fresh snapshot (Head).
var ErrTooFarBehind = errors.New("changefeed: reader is further behind than the feed's retention")

// Cursor is the position of a reader: the (transaction id, seq) of the last row it consumed.
type Cursor struct {
	TxID uint64
	Seq  int64
}

// Less reports whether c is before o.
func (c Cursor) Less(o Cursor) bool {
	return c.TxID < o.TxID || (c.TxID == o.TxID && c.Seq < o.Seq)
}

// Change is one feed row: Op on the row Key of Entity (the tracked table's entity name) in Tenant.
type Change struct {
	Seq    int64
	TxID   uint64
	Tenant tenant.ID
	Entity string
	Key    string
	Op     string
	At     time.Time
}

// Cursor is the position just after c.
func (c Change) Cursor() Cursor { return Cursor{TxID: c.TxID, Seq: c.Seq} }

type row struct {
	Seq    int64     `db:"seq"`
	TxID   string    `db:"tx_id"`
	Tenant string    `db:"tenant_id"`
	Entity string    `db:"entity"`
	Key    string    `db:"key"`
	Op     string    `db:"op"`
	At     time.Time `db:"at"`
}

type position struct {
	TxID string `db:"tx_id"`
	Seq  int64  `db:"seq"`
}

func (p position) cursor() (Cursor, error) {
	id, err := strconv.ParseUint(p.TxID, 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("changefeed: transaction id %q: %w", p.TxID, err)
	}
	return Cursor{TxID: id, Seq: p.Seq}, nil
}

// inSnapshot runs fn in a read-only REPEATABLE READ transaction, so everything fn reads (the horizon, the rows and
// the snapshot's xmin) comes from one snapshot.
func inSnapshot(ctx context.Context, db *sqlx.DB, fn func(tx *sqlx.Tx) error) error {
	tx, err := db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("changefeed: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func horizon(ctx context.Context, tx *sqlx.Tx) (Cursor, error) {
	var p position
	if err := tx.GetContext(ctx, &p, "SELECT tx_id::text AS tx_id, seq FROM changes_horizon"); err != nil {
		return Cursor{}, fmt.Errorf("changefeed: horizon: %w", err)
	}
	return p.cursor()
}

// Head returns the cursor a new reader starts from: after every row it could consume now (and after what retention
// removed). Load what the reader caches after taking the head, so no change in between is missed.
func Head(ctx context.Context, db *sqlx.DB) (Cursor, error) {
	var head Cursor
	err := inSnapshot(ctx, db, func(tx *sqlx.Tx) error {
		h, err := horizon(ctx, tx)
		if err != nil {
			return err
		}
		head = h
		var ps []position
		err = tx.SelectContext(ctx, &ps, `SELECT tx_id::text AS tx_id, seq FROM changes
			WHERE tx_id < pg_snapshot_xmin(pg_current_snapshot())
			ORDER BY tx_id DESC, seq DESC LIMIT 1`)
		if err != nil {
			return fmt.Errorf("changefeed: head: %w", err)
		}
		if len(ps) == 1 {
			c, err := ps[0].cursor()
			if err != nil {
				return err
			}
			if head.Less(c) {
				head = c
			}
		}
		return nil
	})
	return head, err
}

// Read returns up to limit changes after cursor that are safe to consume, in (tx_id, seq) order, and the cursor
// after them (cursor itself when there are none). It returns ErrTooFarBehind when retention removed rows after
// cursor.
func Read(ctx context.Context, db *sqlx.DB, after Cursor, limit int) ([]Change, Cursor, error) {
	var out []Change
	next := after
	err := inSnapshot(ctx, db, func(tx *sqlx.Tx) error {
		h, err := horizon(ctx, tx)
		if err != nil {
			return err
		}
		if after.Less(h) {
			return ErrTooFarBehind
		}
		var rows []row
		err = tx.SelectContext(ctx, &rows, `SELECT seq, tx_id::text AS tx_id, tenant_id, entity, key, op, at FROM changes
			WHERE (tx_id, seq) > ($1::text::xid8, $2)
			  AND tx_id < pg_snapshot_xmin(pg_current_snapshot())
			ORDER BY tx_id, seq
			LIMIT $3`, strconv.FormatUint(after.TxID, 10), after.Seq, limit)
		if err != nil {
			return fmt.Errorf("changefeed: read: %w", err)
		}
		for _, r := range rows {
			c, err := position{TxID: r.TxID, Seq: r.Seq}.cursor()
			if err != nil {
				return err
			}
			out = append(out, Change{
				Seq: r.Seq, TxID: c.TxID, Tenant: tenant.ID(r.Tenant), Entity: r.Entity, Key: r.Key, Op: r.Op, At: r.At,
			})
			next = c
		}
		return nil
	})
	if err != nil {
		return nil, after, err
	}
	return out, next, nil
}

// Prune removes the feed rows older than keep and returns how many it removed. Readers behind the removed rows get
// ErrTooFarBehind.
func Prune(ctx context.Context, db *sqlx.DB, keep time.Duration) (int64, error) {
	var n int64
	err := db.GetContext(ctx, &n, "SELECT rport_prune_changes(make_interval(secs => $1))", keep.Seconds())
	if err != nil {
		return 0, fmt.Errorf("changefeed: prune: %w", err)
	}
	return n, nil
}

// aheadQuery is true when the feed holds transaction ids this database hasn't reached: it was copied from another
// Postgres cluster (pg_dump and restore, logical replication) whose ids were higher. Every new row would sort below
// every reader's cursor, and the feed would deliver nothing.
const aheadQuery = `SELECT greatest((SELECT coalesce(max(tx_id), '0'::xid8) FROM changes), (SELECT tx_id FROM changes_horizon))
	>= pg_snapshot_xmax(pg_current_snapshot())`

// ErrAhead is returned by CheckNotAhead.
var ErrAhead = errors.New(`changefeed: the change feed holds transaction ids this database hasn't reached (was it restored from a dump of another server?); run "rportd migrate" to reset the feed`)

// CheckNotAhead returns ErrAhead when the feed can't work on this database (see ResetIfAhead).
func CheckNotAhead(ctx context.Context, db *sqlx.DB) error {
	var ahead bool
	if err := db.GetContext(ctx, &ahead, aheadQuery); err != nil {
		return fmt.Errorf("changefeed: check: %w", err)
	}
	if ahead {
		return ErrAhead
	}
	return nil
}
