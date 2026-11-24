package chserver

import (
	"context"

	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/server/clients/clienttunnel"
	"github.com/openrport/openrport/server/dispatch"
	"github.com/openrport/openrport/share/models"
)

// liveClients is al's client service as it is when a request is dispatched: tests often set or
// replace al.clientService after building the listener.
type liveClients struct{ al *APIListener }

func (c liveClients) GetByID(ctx context.Context, id string) (*clientdata.Client, error) {
	return c.al.clientService.GetByID(ctx, id)
}

func (c liveClients) StartClientTunnels(ctx context.Context, client *clientdata.Client, remotes []*models.Remote) ([]*clienttunnel.Tunnel, error) {
	return c.al.clientService.StartClientTunnels(ctx, client, remotes)
}

func (c liveClients) FindTunnel(client *clientdata.Client, id string) *clienttunnel.Tunnel {
	return c.al.clientService.FindTunnel(client, id)
}

func (c liveClients) TerminateTunnel(ctx context.Context, client *clientdata.Client, t *clienttunnel.Tunnel, force bool) error {
	return c.al.clientService.TerminateTunnel(ctx, client, t, force)
}

func (c liveClients) SetTunnelACL(ctx context.Context, client *clientdata.Client, t *clienttunnel.Tunnel, acl *string) error {
	return c.al.clientService.SetTunnelACL(ctx, client, t, acl)
}

// useLocalDispatcher gives a hand-built API listener the local dispatcher NewServer would give it,
// unless the test set one.
func useLocalDispatcher(al *APIListener) {
	if al.Server == nil {
		al.Server = &Server{}
	}
	if al.dispatcher != nil {
		return
	}
	grants := al.fileGrants
	if grants == nil {
		grants = dispatch.NewFileGrants()
	}
	al.dispatcher = dispatch.NewLocal(liveClients{al}, grants, testLog)
}
