package notifications

import (
	"context"

	"github.com/openrport/openrport/share/query"
)

// Repository stores the notification log. Every method is scoped to the tenant carried by its context and fails with
// tenant.ErrMissing when there is none.
type Repository interface {
	List(ctx context.Context, options *query.ListOptions) ([]NotificationSummary, error)
	Count(ctx context.Context, options *query.ListOptions) (int, error)
	Details(ctx context.Context, nid string) (NotificationDetails, bool, error)
	Create(ctx context.Context, details NotificationDetails) error
	SetDone(ctx context.Context, details NotificationDetails, out string) error
	SetError(ctx context.Context, details NotificationDetails, out, err string) error
	NotificationStream(target Target) chan NotificationDetails
	Close() error
}
