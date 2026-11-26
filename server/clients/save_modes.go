package clients

import (
	"context"

	"github.com/pohcee/procy-server/server/clients/clientdata"
)

// AdminFieldsSaver is a ClientStore shared by several server instances that can write only the fields admins set on
// a client (its allowed user groups) to its stored record, leaving the rest as stored. It returns an error for a
// client that is not stored (deleted meanwhile): it is not brought back.
type AdminFieldsSaver interface {
	SaveAdminFields(ctx context.Context, c *clientdata.Client) error
}

// ACLKeepingSaver is a ClientStore shared by several server instances that can save a client keeping the stored
// allowed user groups (and setting them on c), for saves that report what the client did or said: they must not
// write back an ACL another instance changed meanwhile.
type ACLKeepingSaver interface {
	SaveKeepingACL(ctx context.Context, c *clientdata.Client) error
}

// SaveAdmin saves a client after an admin changed its allowed user groups: unlike Save, a store shared with other
// instances writes the ACL as given.
func (r *ClientRepository) SaveAdmin(ctx context.Context, cl *clientdata.Client) error {
	return r.save(ctx, cl, true)
}

// storeSave writes cl to store: saves by an admin as given, other saves keeping the stored ACL when the store can.
func storeSave(ctx context.Context, store ClientStore, cl *clientdata.Client, admin bool) error {
	if !admin {
		if k, ok := store.(ACLKeepingSaver); ok {
			return k.SaveKeepingACL(ctx, cl)
		}
	}
	return store.Save(ctx, cl)
}
