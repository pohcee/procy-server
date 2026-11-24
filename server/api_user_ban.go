package chserver

import (
	"context"

	"github.com/openrport/openrport/share/tenant"
)

// userBanKey is the ban-list key of a username: banned users are kept per tenant, so a ban in one tenant
// never locks out the same username in another. A context without a tenant gets its own namespace ("" is never
// a tenant ID). Banned IPs stay global.
func userBanKey(ctx context.Context, username string) string {
	tid, _ := tenant.FromContext(ctx)
	return string(tid) + "\x00" + username
}

func (al *APIListener) banUser(ctx context.Context, username string) {
	al.bannedUsers.Add(userBanKey(ctx, username))
}

func (al *APIListener) isUserBanned(ctx context.Context, username string) bool {
	return al.bannedUsers.IsBanned(userBanKey(ctx, username))
}
