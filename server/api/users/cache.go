package users

import (
	"context"
	"fmt"
	"sync"

	"github.com/openrport/openrport/share/tenant"
)

// UserCache is in memory user cache with thread-safe loading. Users are kept per tenant: a lookup in one tenant
// never sees another tenant's users.
type UserCache struct {
	byTenant map[tenant.ID]map[string]*User
	mu       sync.RWMutex
}

// NewUserCache returns a cache holding initUsers in the default tenant.
func NewUserCache(initUsers []*User) *UserCache {
	r := &UserCache{byTenant: map[tenant.ID]map[string]*User{}}
	r.load(tenant.Default, initUsers)
	return r
}

func cacheTenant(ctx context.Context) (tenant.ID, error) {
	id, err := tenant.FromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("users: %w", err)
	}
	return id, nil
}

// Load replaces the users of the context's tenant with given users
func (r *UserCache) Load(ctx context.Context, users []*User) error {
	tid, err := cacheTenant(ctx)
	if err != nil {
		return err
	}
	r.load(tid, users)
	return nil
}

func (r *UserCache) load(tid tenant.ID, users []*User) {
	m := make(map[string]*User, len(users))
	for _, u := range users {
		m[u.Username] = u
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byTenant[tid] = m
}

// GetByUsername returns the context tenant's user with the given username or nil
func (r *UserCache) GetByUsername(ctx context.Context, username string) (*User, error) {
	tid, err := cacheTenant(ctx)
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byTenant[tid][username], nil
}

func (r *UserCache) GetAll(ctx context.Context) ([]*User, error) {
	tid, err := cacheTenant(ctx)
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	users := make([]*User, 0, len(r.byTenant[tid]))
	for _, u := range r.byTenant[tid] {
		users = append(users, u)
	}
	return users, nil
}
