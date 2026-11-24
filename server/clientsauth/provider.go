package clientsauth

import (
	"context"

	"github.com/openrport/openrport/share/enums"
	"github.com/openrport/openrport/share/query"
)

// Provider serves client authentication credentials. The file, database and single providers read storage
// the admin owns, so they serve only the default tenant: any other tenant gets tenant.ErrNotSupported.
type Provider interface {
	// Get returns client authentication credentials from provider or nil
	Get(ctx context.Context, id string) (*ClientAuth, error)
	// GetFiltered returns authentication credentials and total count filtered
	GetFiltered(ctx context.Context, filter *query.ListOptions) ([]*ClientAuth, int, error)
	// Add returns true if the client auth was added and false if it already exists
	Add(ctx context.Context, client *ClientAuth) (bool, error)
	// Delete returns client auth by id
	Delete(ctx context.Context, id string) error
	// IsWriteable returns true if provider is writeable
	IsWriteable() bool
	// Source returns a provider source
	Source() enums.ProviderSource
}
