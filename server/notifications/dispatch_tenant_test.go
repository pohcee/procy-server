package notifications

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/share/refs"
	"github.com/pohcee/procy-server/share/tenant"
)

type recordingDispatcher struct{ got []tenant.ID }

func (r *recordingDispatcher) Dispatch(ctx context.Context, _ refs.Identifiable, _ NotificationData) (refs.Identifiable, error) {
	id, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	r.got = append(r.got, id)
	return nil, nil
}

// Code outside this repo (the alerting plugin) may dispatch with a context of its own; it then
// gets the tenant the server runs it for. A context that has a tenant keeps it.
func TestWithTenantFallbackFillsMissingTenantOnly(t *testing.T) {
	inner := &recordingDispatcher{}
	d := WithTenantFallback(inner, tenant.Default)

	_, err := d.Dispatch(context.Background(), nil, NotificationData{})
	require.NoError(t, err)
	_, err = d.Dispatch(tenant.WithID(context.Background(), "a"), nil, NotificationData{})
	require.NoError(t, err)
	assert.Equal(t, []tenant.ID{tenant.Default, "a"}, inner.got)
}
