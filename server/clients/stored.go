package clients

import (
	"time"

	"github.com/pohcee/procy-server/server/clients/clientdata"
	"github.com/pohcee/procy-server/share/logger"
)

// StoredClient is a client in the form a ClientStore keeps it: the columns a store queries, and every other
// persisted field as the JSON document Details. Other ClientStore implementations use it to keep the same
// documents as the SQLite store, so data can be copied between them unchanged.
type StoredClient struct {
	ID           string
	ClientAuthID string
	// DisconnectedAt is nil for a connected client.
	DisconnectedAt *time.Time
	Details        string
}

// EncodeStoredClient returns c as a ClientStore keeps it.
func EncodeStoredClient(c *clientdata.Client) (StoredClient, error) {
	s := convertToSqlite(c)
	details, err := s.Details.Value()
	if err != nil {
		return StoredClient{}, err
	}
	res := StoredClient{ID: s.ID, ClientAuthID: s.ClientAuthID, Details: details.(string)}
	if s.DisconnectedAt.Valid {
		at := s.DisconnectedAt.Time
		res.DisconnectedAt = &at
	}
	return res, nil
}

// DecodeStoredClient returns the client kept as s, logging with l.
func DecodeStoredClient(s StoredClient, l *logger.Logger) (*clientdata.Client, error) {
	row := &clientSqlite{ID: s.ID, ClientAuthID: s.ClientAuthID, Details: &clientDetails{}}
	if err := row.Details.Scan(s.Details); err != nil {
		return nil, err
	}
	if s.DisconnectedAt != nil {
		row.DisconnectedAt.Time = *s.DisconnectedAt
		row.DisconnectedAt.Valid = true
	}
	return row.convert(l), nil
}
