package dispatch

import (
	"context"

	"golang.org/x/crypto/ssh"

	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/server/clients/clienttunnel"
	"github.com/openrport/openrport/share/comm"
	errors3 "github.com/openrport/openrport/share/errors"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/models"
)

// LocalClients is what Local needs from the client service: the clients connected to this process
// and their tunnels. clients.ClientService satisfies it.
type LocalClients interface {
	GetByID(ctx context.Context, id string) (*clientdata.Client, error)
	StartClientTunnels(ctx context.Context, client *clientdata.Client, remotes []*models.Remote) ([]*clienttunnel.Tunnel, error)
	FindTunnel(c *clientdata.Client, id string) *clienttunnel.Tunnel
	TerminateTunnel(ctx context.Context, c *clientdata.Client, t *clienttunnel.Tunnel, force bool) error
	SetTunnelACL(ctx context.Context, c *clientdata.Client, t *clienttunnel.Tunnel, aclStr *string) error
}

// Local is the Dispatcher for clients connected to this process: it sends each request over the
// client's SSH connection.
type Local struct {
	clients LocalClients
	grants  *FileGrants
	log     *logger.Logger
}

var _ Dispatcher = (*Local)(nil)

// NewLocal returns a Local that finds clients in clients and grants uploaded files in grants (the
// grants the client listener's SFTP server checks).
func NewLocal(clients LocalClients, grants *FileGrants, log *logger.Logger) *Local {
	return &Local{clients: clients, grants: grants, log: log}
}

// client returns the client and its connection; ErrClientNotConnected when either is missing.
func (d *Local) client(ctx context.Context, clientID string) (*clientdata.Client, ssh.Conn, error) {
	client, err := d.clients.GetByID(ctx, clientID)
	if err != nil {
		return nil, nil, err
	}
	if client == nil {
		return nil, nil, ErrClientNotConnected
	}
	conn := client.GetConnection()
	if conn == nil {
		return nil, nil, ErrClientNotConnected
	}
	return client, conn, nil
}

func (d *Local) tunnel(ctx context.Context, clientID, tunnelID string) (*clientdata.Client, *clienttunnel.Tunnel, error) {
	client, _, err := d.client(ctx, clientID)
	if err != nil {
		return nil, nil, err
	}
	t := d.clients.FindTunnel(client, tunnelID)
	if t == nil {
		return nil, nil, ErrTunnelNotFound
	}
	return client, t, nil
}

func (d *Local) RunJob(ctx context.Context, clientID string, job *models.Job) (*comm.RunCmdResponse, error) {
	_, conn, err := d.client(ctx, clientID)
	if err != nil {
		return nil, err
	}
	resp := &comm.RunCmdResponse{}
	if err := comm.SendRequestAndGetResponse(conn, comm.RequestTypeRunCmd, job, resp, d.log); err != nil {
		return nil, err
	}
	return resp, nil
}

func (d *Local) Upload(ctx context.Context, clientID string, file *models.UploadedFile) (*models.UploadResponse, error) {
	client, conn, err := d.client(ctx, clientID)
	if err != nil {
		return nil, err
	}
	if cfg := client.GetFileReceptionConfig(); cfg != nil && !cfg.Enabled {
		return nil, errors3.ErrUploadsDisabled
	}
	// The client fetches the file over SFTP while it handles the request.
	revoke := d.grants.Grant(conn, file.SourceFilePath)
	defer revoke()

	resp := &models.UploadResponse{}
	err = comm.SendRequestAndGetResponse(conn, comm.RequestTypeUpload, file, resp, d.log)
	return resp, err
}

func (d *Local) IsTunnelAllowed(ctx context.Context, clientID, remote string) (bool, error) {
	_, conn, err := d.client(ctx, clientID)
	if err != nil {
		return false, err
	}
	return clienttunnel.IsAllowed(remote, conn, d.log)
}

func (d *Local) CheckPort(ctx context.Context, clientID string, req *comm.CheckPortRequest) (*comm.CheckPortResponse, error) {
	_, conn, err := d.client(ctx, clientID)
	if err != nil {
		return nil, err
	}
	resp := &comm.CheckPortResponse{}
	if err := comm.SendRequestAndGetResponse(conn, comm.RequestTypeCheckPort, req, resp, d.log); err != nil {
		return nil, err
	}
	return resp, nil
}

func (d *Local) StartTunnels(ctx context.Context, clientID string, remotes []*models.Remote) ([]*clienttunnel.Tunnel, error) {
	client, _, err := d.client(ctx, clientID)
	if err != nil {
		return nil, err
	}
	return d.clients.StartClientTunnels(ctx, client, remotes)
}

func (d *Local) CloseTunnel(ctx context.Context, clientID, tunnelID string, force bool) error {
	client, t, err := d.tunnel(ctx, clientID, tunnelID)
	if err != nil {
		return err
	}
	return d.clients.TerminateTunnel(ctx, client, t, force)
}

func (d *Local) SetTunnelACL(ctx context.Context, clientID, tunnelID string, acl *string) error {
	client, t, err := d.tunnel(ctx, clientID, tunnelID)
	if err != nil {
		return err
	}
	return d.clients.SetTunnelACL(ctx, client, t, acl)
}

func (d *Local) RefreshUpdatesStatus(ctx context.Context, clientID string) error {
	_, conn, err := d.client(ctx, clientID)
	if err != nil {
		return err
	}
	return comm.SendRequestAndGetResponse(conn, comm.RequestTypeRefreshUpdatesStatus, nil, nil, d.log)
}

// attributesResponse is the client's answer to an attributes update; its content is not used.
type attributesResponse struct {
	OK string `json:"ok"`
}

func (d *Local) UpdateAttributes(ctx context.Context, clientID string, attributes models.Attributes) error {
	_, conn, err := d.client(ctx, clientID)
	if err != nil {
		return err
	}
	return comm.SendRequestAndGetResponse(conn, comm.RequestTypeUpdateClientAttributes, attributes, &attributesResponse{}, d.log)
}
