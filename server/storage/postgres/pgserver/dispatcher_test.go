package pgserver

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/openrport/openrport/server/dispatch"
)

func TestNotConnectedDispatcherEveryMethod(t *testing.T) {
	d := NotConnectedDispatcher{}
	ctx := context.Background()
	_, e1 := d.RunJob(ctx, "c", nil)
	_, e2 := d.Upload(ctx, "c", nil)
	_, e3 := d.IsTunnelAllowed(ctx, "c", "127.0.0.1:22")
	_, e4 := d.CheckPort(ctx, "c", nil)
	_, e5 := d.StartTunnels(ctx, "c", nil)
	errs := []error{e1, e2, e3, e4, e5,
		d.CloseTunnel(ctx, "c", "t", false), d.SetTunnelACL(ctx, "c", "t", nil),
		d.RefreshUpdatesStatus(ctx, "c"), d.UpdateAttributes(ctx, "c", struct {
			Tags   []string          `json:"tags"`
			Labels map[string]string `json:"labels"`
		}{})}
	for i, err := range errs {
		assert.True(t, errors.Is(err, dispatch.ErrClientNotConnected), "call %d: %v", i, err)
	}
}
