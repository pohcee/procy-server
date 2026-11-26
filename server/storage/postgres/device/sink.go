// Package device is the storage of a device process (a node with [storage] role = "device"): it connects as the
// device database role, so it reads the device path only through the device_* lookup functions and writes what
// devices report only through the outbox, whose Sink calls the ingest functions (migration 1100).
package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/server/storage/outbox"
)

// ingestFunctions are the database functions items of each kind are shipped through.
var ingestFunctions = map[outbox.Kind]string{
	outbox.KindJobResult:       "ingest_job_results",
	outbox.KindMeasurement:     "ingest_measurements",
	outbox.KindConnectionEvent: "ingest_connection_events",
	outbox.KindAudit:           "ingest_audit",
	outbox.KindClientState:     "report_client_state",
	outbox.KindUsage:           "ingest_usage",
}

// Sink ships outbox items to Postgres through the ingest functions, one call per batch.
type Sink struct {
	DB *sqlx.DB
}

var _ outbox.Sink = Sink{}

// Ship calls the ingest function of kind with items (each payload a JSON object with tenant_id and key). When the
// call fails for a reason in the data (SQLSTATE classes 22, 23 and 54, as a value Postgres can't store), the items
// are shipped one by one, and one that fails alone is reported rejected: a bad item must not hold up the others.
func (s Sink) Ship(ctx context.Context, kind outbox.Kind, items []outbox.Item) ([]outbox.Status, error) {
	fn, ok := ingestFunctions[kind]
	if !ok {
		return nil, fmt.Errorf("device: no ingest function for %q", kind)
	}
	out, err := s.call(ctx, fn, items)
	if err == nil || !inData(err) {
		return out, err
	}
	out = out[:0]
	for _, it := range items {
		one, err := s.call(ctx, fn, []outbox.Item{it})
		switch {
		case err == nil:
			out = append(out, one...)
		case inData(err):
			out = append(out, outbox.Status{Key: it.Key, Outcome: "rejected: " + err.Error()})
		default:
			return out, err
		}
	}
	return out, nil
}

func (s Sink) call(ctx context.Context, fn string, items []outbox.Item) ([]outbox.Status, error) {
	batch := make([]json.RawMessage, len(items))
	for i, it := range items {
		batch[i] = it.Payload
	}
	b, err := json.Marshal(batch)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Key    string `db:"out_key"`
		Status string `db:"out_status"`
	}
	// fn is one of the constants above
	if err := s.DB.SelectContext(ctx, &rows, "SELECT out_key, out_status FROM "+fn+"($1::jsonb)", string(b)); err != nil {
		return nil, fmt.Errorf("device: %s: %w", fn, err)
	}
	out := make([]outbox.Status, len(rows))
	for i, r := range rows {
		out[i] = outbox.Status{Key: r.Key, Outcome: r.Status}
	}
	return out, nil
}

// inData reports whether err is a Postgres error caused by the data sent (data exception, integrity constraint
// violation, program limit exceeded), which sending the same data again can't fix.
func inData(err error) bool {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || len(pe.Code) < 2 {
		return false
	}
	switch pe.Code[:2] {
	case "22", "23", "54":
		return true
	}
	return false
}
