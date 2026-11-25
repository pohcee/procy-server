package clienttunnel

import (
	"context"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"

	"github.com/openrport/openrport/server/metering"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/models"
)

type TunnelProtocol interface {
	Start(ctx context.Context) error
	Terminate(force bool) error
	LastActive() time.Time
	SetACL(*TunnelACL)
}

type MultiProtocolTunnel struct {
	Protocols []TunnelProtocol
}

// Start starts the tunnel; when ctx ends, its byte counting ends.
func (t *Tunnel) Start(ctx context.Context) error {
	if err := t.TunnelProtocol.Start(ctx); err != nil {
		t.Bytes.Close()
		return err
	}
	if t.Bytes != nil {
		t.stopCounting = context.AfterFunc(ctx, t.Bytes.Close)
	}
	return nil
}

func (mt *MultiProtocolTunnel) Start(ctx context.Context) error {
	for _, tp := range mt.Protocols {
		err := tp.Start(ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

func (mt *MultiProtocolTunnel) Terminate(force bool) error {
	var result error
	for _, tp := range mt.Protocols {
		err := tp.Terminate(force)
		if err != nil {
			result = multierror.Append(result, err)
		}
	}
	return result
}

func (mt *MultiProtocolTunnel) LastActive() time.Time {
	var result time.Time
	for _, tp := range mt.Protocols {
		v := tp.LastActive()
		if v.After(result) {
			result = v
		}
	}
	return result
}

func (mt *MultiProtocolTunnel) SetACL(acl *TunnelACL) {
	for _, tp := range mt.Protocols {
		tp.SetACL(acl)
	}
}

// TODO(m-terel): Refactor to use separate models for representation and business logic.
// Tunnel represents active remote proxy connection
type Tunnel struct {
	ID string `json:"id"`

	models.Remote

	TunnelProtocol      `json:"-"`
	InternalTunnelProxy *InternalTunnelProxy `json:"-"`
	CreatedAt           time.Time            `json:"created_at"`
	// TunnelHost is the host name of the server holding the tunnel, set only when several servers share the
	// clients (a cluster); links to the tunnel use it instead of the API's tunnel_host.
	TunnelHost string `json:"tunnel_host,omitempty"`
	// Bytes counts what the tunnel carried, only when usage metering is on (it is omitted otherwise).
	Bytes *metering.TunnelBytes `json:"bytes,omitempty"`
	// stopCounting releases the context hook that ends the counting (set by Start when metered)
	stopCounting func() bool
}

// Terminate stops the tunnel; once it is stopped its byte counting ends.
func (t *Tunnel) Terminate(force bool) error {
	if err := t.TunnelProtocol.Terminate(force); err != nil {
		return err
	}
	t.Bytes.Close()
	if t.stopCounting != nil {
		t.stopCounting()
	}
	return nil
}

func NewTunnel(logger *logger.Logger, ssh ssh.Conn, id string, remote models.Remote, acl *TunnelACL) (*Tunnel, error) {
	return NewMeteredTunnel(logger, ssh, id, remote, acl, nil)
}

// NewMeteredTunnel is NewTunnel counting the tunnel's bytes in bytes (nil: not counted). The counting ends when the
// tunnel is terminated or the context it was started with ends.
func NewMeteredTunnel(logger *logger.Logger, ssh ssh.Conn, id string, remote models.Remote, acl *TunnelACL, bytes *metering.TunnelBytes) (*Tunnel, error) {
	logger = logger.Fork("tunnel#%s:%s", id, remote)
	logger.Debugf("new tunnel with remote = %#v", remote)

	var tunnelProtocol TunnelProtocol
	switch remote.Protocol {
	case models.ProtocolUDP:
		u := newTunnelUDP(logger, ssh, remote, acl)
		u.bytes = bytes
		tunnelProtocol = u
	case models.ProtocolTCP:
		t := newTunnelTCP(logger, ssh, remote, acl)
		t.bytes = bytes
		tunnelProtocol = t
	case models.ProtocolTCPUDP:
		t := newTunnelTCP(logger, ssh, remote, acl)
		t.bytes = bytes
		u := newTunnelUDP(logger, ssh, remote, acl)
		u.bytes = bytes
		tunnelProtocol = &MultiProtocolTunnel{Protocols: []TunnelProtocol{t, u}}
	default:
		bytes.Close()
		return nil, errors.Errorf("unsupported protocol %q", remote.Protocol)
	}

	return &Tunnel{
		Remote:         remote,
		ID:             id,
		TunnelProtocol: tunnelProtocol,
		CreatedAt:      time.Now(),
		Bytes:          bytes,
	}, nil
}
