package chserver

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/openrport/openrport/share/tenant"
)

func TestNewAPIListenerRequiresTenant(t *testing.T) {
	_, err := NewAPIListener(context.Background(), &Server{}, "")
	assert.ErrorIs(t, err, tenant.ErrMissing)
}
