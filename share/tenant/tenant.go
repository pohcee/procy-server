// Package tenant carries the tenant a request or task belongs to.
//
// Every tenant-scoped repository reads the tenant from the context and refuses to run without one.
// A standalone server has a single, invisible tenant: Default.
package tenant

import (
	"context"
	"errors"
	"fmt"
)

// ID identifies a tenant.
type ID string

// Default is the only tenant of a standalone server.
const Default ID = "default"

// ErrMissing is returned when a context carries no tenant.
var ErrMissing = errors.New("tenant: no tenant in context")

// ErrNotSupported is returned by storage that serves only the default tenant (for example users or client
// credentials from a file or admin-defined tables) when asked for another tenant.
var ErrNotSupported = errors.New("tenant: only the default tenant is supported")

// RequireDefault returns nil if ctx carries the default tenant, an error wrapping ErrMissing if it carries none,
// and one wrapping ErrNotSupported for any other tenant.
func RequireDefault(ctx context.Context) error {
	id, err := FromContext(ctx)
	if err != nil {
		return err
	}
	if id != Default {
		return fmt.Errorf("%w: %q", ErrNotSupported, id)
	}
	return nil
}

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
