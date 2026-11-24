// Package tenant carries the tenant a request or task belongs to.
//
// Every tenant-scoped repository reads the tenant from the context and refuses to run without one.
// A standalone server has a single, invisible tenant: Default.
package tenant

import (
	"context"
	"errors"
)

// ID identifies a tenant.
type ID string

// Default is the only tenant of a standalone server.
const Default ID = "default"

// ErrMissing is returned when a context carries no tenant.
var ErrMissing = errors.New("tenant: no tenant in context")

type ctxKey struct{}

// WithID returns a copy of ctx carrying the tenant id. An empty id counts as no tenant.
func WithID(ctx context.Context, id ID) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the tenant carried by ctx, or ErrMissing.
func FromContext(ctx context.Context) (ID, error) {
	id, _ := ctx.Value(ctxKey{}).(ID)
	if id == "" {
		return "", ErrMissing
	}
	return id, nil
}
