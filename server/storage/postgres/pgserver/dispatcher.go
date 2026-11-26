package pgserver

import (
	"context"
	"fmt"

	"github.com/pohcee/procy-server/server/clients/clienttunnel"
	"github.com/pohcee/procy-server/server/dispatch"
	"github.com/pohcee/procy-server/share/comm"
	"github.com/pohcee/procy-server/share/models"
)

// ErrNotConnected is what every NotConnectedDispatcher call returns. It wraps dispatch.ErrClientNotConnected, so the
// API treats it as an offline client.
var ErrNotConnected = fmt.Errorf("%w: this server does not hold client connections yet", dispatch.ErrClientNotConnected)

// NotConnectedDispatcher is the dispatcher of a server without a client listener (storage role "api") until work can be passed to the node holding the client: every
// client is unreachable.
type NotConnectedDispatcher struct{}

var _ dispatch.Dispatcher = NotConnectedDispatcher{}

func (NotConnectedDispatcher) RunJob(context.Context, string, *models.Job) (*comm.RunCmdResponse, error) {
	return nil, ErrNotConnected
}

func (NotConnectedDispatcher) Upload(context.Context, string, *models.UploadedFile) (*models.UploadResponse, error) {
	return nil, ErrNotConnected
}

func (NotConnectedDispatcher) IsTunnelAllowed(context.Context, string, string) (bool, error) {
	return false, ErrNotConnected
}

func (NotConnectedDispatcher) CheckPort(context.Context, string, *comm.CheckPortRequest) (*comm.CheckPortResponse, error) {
	return nil, ErrNotConnected
}

func (NotConnectedDispatcher) StartTunnels(context.Context, string, []*models.Remote) ([]*clienttunnel.Tunnel, error) {
	return nil, ErrNotConnected
}

func (NotConnectedDispatcher) CloseTunnel(context.Context, string, string, bool) error {
	return ErrNotConnected
}

func (NotConnectedDispatcher) SetTunnelACL(context.Context, string, string, *string) error {
	return ErrNotConnected
}

func (NotConnectedDispatcher) RefreshUpdatesStatus(context.Context, string) error {
	return ErrNotConnected
}

func (NotConnectedDispatcher) UpdateAttributes(context.Context, string, models.Attributes) error {
	return ErrNotConnected
}
