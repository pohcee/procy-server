package schedule

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/share/tenant"
)

func TestCronCallbackInheritsBaseContext(t *testing.T) {
	c := newCron(tenant.WithID(context.Background(), tenant.Default))
	got := make(chan tenant.ID, 1)
	require.NoError(t, c.Add("s1", "@every 1s", func(ctx context.Context, _ string) {
		id, _ := tenant.FromContext(ctx)
		select {
		case got <- id:
		default:
		}
	}))
	defer c.Remove("s1")
	select {
	case id := <-got:
		assert.Equal(t, tenant.Default, id)
	case <-time.After(3 * time.Second):
		t.Fatal("cron callback did not run")
	}
}
