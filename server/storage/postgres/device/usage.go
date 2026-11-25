package device

import (
	"context"

	"github.com/openrport/openrport/server/metering"
	"github.com/openrport/openrport/server/storage/outbox"
)

// Usage is the metering.Writer of a device process: each record goes into the outbox (kind "usage", never
// dropped), whose Shipper calls ingest_usage.
type Usage struct {
	Outbox *outbox.Outbox
}

var _ metering.Writer = Usage{}

// WriteUsage adds recs to the outbox; a record whose key is there already is not added again.
func (u Usage) WriteUsage(ctx context.Context, recs []metering.Record) error {
	for _, r := range recs {
		p, err := r.Payload()
		if err != nil {
			return err
		}
		if err := u.Outbox.Add(ctx, outbox.KindUsage, r.Tenant, r.Key, p); err != nil {
			return err
		}
	}
	return nil
}
