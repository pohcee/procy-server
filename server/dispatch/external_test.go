package dispatch_test

import (
	"context"

	"github.com/openrport/openrport/server/clients/clienttunnel"
	"github.com/openrport/openrport/server/dispatch"
	"github.com/openrport/openrport/share/comm"
	"github.com/openrport/openrport/share/models"
)

// externalDispatcher implements the interface from outside the package without embedding it,
// which proves another Go module can provide a Dispatcher.
type externalDispatcher struct{}

func (externalDispatcher) RunJob(context.Context, string, *models.Job) (*comm.RunCmdResponse, error) {
	return nil, nil
}
func (externalDispatcher) Upload(context.Context, string, *models.UploadedFile) (*models.UploadResponse, error) {
	return nil, nil
}
func (externalDispatcher) IsTunnelAllowed(context.Context, string, string) (bool, error) {
	return false, nil
}
func (externalDispatcher) CheckPort(context.Context, string, *comm.CheckPortRequest) (*comm.CheckPortResponse, error) {
	return nil, nil
}
func (externalDispatcher) StartTunnels(context.Context, string, []*models.Remote) ([]*clienttunnel.Tunnel, error) {
	return nil, nil
}
func (externalDispatcher) CloseTunnel(context.Context, string, string, bool) error     { return nil }
func (externalDispatcher) SetTunnelACL(context.Context, string, string, *string) error { return nil }
func (externalDispatcher) RefreshUpdatesStatus(context.Context, string) error          { return nil }
func (externalDispatcher) UpdateAttributes(context.Context, string, models.Attributes) error {
	return nil
}

var (
	_ dispatch.Dispatcher = externalDispatcher{}
	_ dispatch.Dispatcher = (*dispatch.Local)(nil)
)
