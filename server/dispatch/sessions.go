package dispatch

import "context"

// Sessions records which server process holds each client's connection (the cluster's session registry). The
// client listener reports every connection it accepts; nil Sessions (standalone) records nothing.
type Sessions interface {
	// Connected records that this process holds a new connection of clientID (tenant from ctx). closeConn closes
	// the connection; it is called when a newer connection of the client, to any process, replaced this one. end
	// records that the connection ended; call it once, when it does.
	Connected(ctx context.Context, clientID string, closeConn func()) (end func(), err error)
}
