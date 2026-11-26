package clients

import (
	"context"
	"fmt"

	"github.com/pohcee/procy-server/server/clients/clientdata"
	"github.com/pohcee/procy-server/share/logger"
)

// LoadInitialClients returns an initial Client Repository state populated with clients from the internal storage.
// The clients stored as connected are marked disconnected and saved, unless p is wrapped by KeepConnectionStateOnLoad.
func LoadInitialClients(ctx context.Context, p ClientStore, logger *logger.Logger) ([]*clientdata.Client, error) {
	logger.Debugf("loading existing clients")

	// setup a logger for the clients
	clientLogger := logger.Fork("client")

	all, err := p.GetAll(ctx, clientLogger)
	if err != nil {
		return nil, fmt.Errorf("failed to get clients: %v", err)
	}

	logger.Debugf("loaded %d clients", len(all))

	if _, keep := p.(connectionStateKeeper); keep {
		return all, nil
	}

	// mark previously connected clients as disconnected with current time
	now := clientdata.Now()

	for _, client := range all {
		if client.IsConnected() {
			client.SetDisconnectedAt(&now)
			err := p.Save(ctx, client)
			if err != nil {
				return nil, fmt.Errorf("failed to save client: %v", err)
			}
		}
	}

	return all, nil
}

// connectionStateKeeper marks a ClientStore wrapped by KeepConnectionStateOnLoad.
type connectionStateKeeper interface {
	keepsConnectionStateOnLoad()
}

// KeepConnectionStateOnLoad returns store wrapped so that LoadInitialClients leaves the connection state of the
// loaded clients as stored, instead of marking (and saving) the connected ones disconnected. That marking is right
// for the server instance holding the client connections, as none survive its restart, and wrong for one that holds
// none while sharing the store with the instance that does. The wrapper forwards every method, and Get when store
// is a ClientGetter.
func KeepConnectionStateOnLoad(store ClientStore) ClientStore {
	if g, ok := store.(ClientGetter); ok {
		return keepConnectionStateGetter{keepConnectionState{store}, g}
	}
	return keepConnectionState{store}
}

type keepConnectionState struct{ ClientStore }

func (keepConnectionState) keepsConnectionStateOnLoad() {}

type keepConnectionStateGetter struct {
	keepConnectionState
	getter ClientGetter
}

func (s keepConnectionStateGetter) Get(ctx context.Context, id string, l *logger.Logger) (*clientdata.Client, error) {
	return s.getter.Get(ctx, id, l)
}

// Save of a store wrapped by KeepConnectionStateOnLoad writes only what an admin sets on a client
// (AdminFieldsSaver) when the store supports it: the instance holding no connections must not write the connection
// state or what the clients report, which the instance holding them owns.
func (s keepConnectionState) Save(ctx context.Context, c *clientdata.Client) error {
	if a, ok := s.ClientStore.(AdminFieldsSaver); ok {
		return a.SaveAdminFields(ctx, c)
	}
	return s.ClientStore.Save(ctx, c)
}
