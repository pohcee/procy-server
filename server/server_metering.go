package chserver

import (
	"context"

	"github.com/pohcee/procy-server/server/clients"
	"github.com/pohcee/procy-server/server/metering"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/tenant"
)

// MeteringOpts turn usage metering on (Postgres nodes with [metering] enabled).
type MeteringOpts struct {
	// Writer stores what the node counted: the outbox of a device process, or ingest_usage directly.
	Writer metering.Writer
	// Reader reads the usage for the API; nil on nodes without an API (device processes).
	Reader metering.Reader
}

// newMeter returns the meter of a server with metering on: it counts on node s.cluster.NodeID() and, when the
// server has a client listener, samples the clients connected to this process.
func (s *Server) newMeter(o *MeteringOpts, withDevices bool) *metering.Meter {
	mo := metering.Options{
		Node:          s.cluster.NodeID(),
		Writer:        o.Writer,
		FlushInterval: s.config.Metering.FlushInterval,
		Log:           logger.NewLogger("metering", s.config.Logging.LogOutput, s.config.Logging.LogLevel),
	}
	if withDevices {
		mo.Devices = connectedDevices(s.clientService.GetRepo())
	}
	return metering.New(mo)
}

// connectedDevices lists the clients with a connection to this process, per tenant the repository holds clients
// of. Clients only loaded (connected to another node, or remembered disconnected) are not counted.
func connectedDevices(repo *clients.ClientRepository) metering.ConnectedDevices {
	return func(ctx context.Context) map[tenant.ID][]string {
		out := map[tenant.ID][]string{}
		for _, tid := range repo.Tenants() {
			for _, c := range repo.GetAllActiveClients(tenant.WithID(ctx, tid)) {
				if c.GetConnection() != nil {
					out[tid] = append(out[tid], c.GetID())
				}
			}
		}
		return out
	}
}
