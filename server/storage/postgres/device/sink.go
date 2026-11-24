// Package device is the storage of a device process (a node with [storage] role = "device"): it connects as the
// device database role, so it reads the device path only through the device_* lookup functions and writes what
// devices report only through the outbox, whose Sink calls the ingest functions (migration 1100).
package device

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/storage/outbox"
)

// ingestFunctions are the database functions items of each kind are shipped through.
var ingestFunctions = map[outbox.Kind]string{
	outbox.KindJobResult:       "ingest_job_results",
	outbox.KindMeasurement:     "ingest_measurements",
	outbox.KindConnectionEvent: "ingest_connection_events",
	outbox.KindAudit:           "ingest_audit",
	outbox.KindClientState:     "report_client_state",
}

// Sink ships outbox items to Postgres through the ingest functions, one call per batch.
type Sink struct {
	DB *sqlx.DB
}

var _ outbox.Sink = Sink{}

// Ship calls the ingest function of kind with items (each payload a JSON object with tenant_id and key).
func (s Sink) Ship(ctx context.Context, kind outbox.Kind, items []outbox.Item) ([]outbox.Status, error) {
	fn, ok := ingestFunctions[kind]
	if !ok {
		return nil, fmt.Errorf("device: no ingest function for %q", kind)
	}
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
