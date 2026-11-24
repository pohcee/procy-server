package tenant_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/share/tenant"
)

func TestFromContextMissing(t *testing.T) {
	_, err := tenant.FromContext(context.Background())
	assert.ErrorIs(t, err, tenant.ErrMissing)
}

func TestWithIDRoundTrip(t *testing.T) {
	id, err := tenant.FromContext(tenant.WithID(context.Background(), "acme"))
	require.NoError(t, err)
	assert.Equal(t, tenant.ID("acme"), id)
}

func TestWithEmptyIDIsMissing(t *testing.T) {
	_, err := tenant.FromContext(tenant.WithID(context.Background(), ""))
	assert.ErrorIs(t, err, tenant.ErrMissing)
}

func TestDefaultIsDefault(t *testing.T) {
	assert.Equal(t, tenant.ID("default"), tenant.Default)
}

func TestRequireDefault(t *testing.T) {
	assert.NoError(t, tenant.RequireDefault(tenant.WithID(context.Background(), tenant.Default)))
	assert.ErrorIs(t, tenant.RequireDefault(context.Background()), tenant.ErrMissing)
	err := tenant.RequireDefault(tenant.WithID(context.Background(), "acme"))
	assert.ErrorIs(t, err, tenant.ErrNotSupported)
	assert.Contains(t, err.Error(), "acme")
}
