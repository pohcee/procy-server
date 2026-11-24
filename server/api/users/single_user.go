package users

import (
	"context"
	"net/http"

	errors2 "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/share/enums"
	"github.com/openrport/openrport/share/tenant"
)

// StaticProvider serves the single user from the config. It is single-tenant: every method accepts only the
// default tenant (tenant.RequireDefault).
type StaticProvider struct {
	*UserCache
}

func NewStaticProvider(users []*User) *StaticProvider {
	return &StaticProvider{
		UserCache: NewUserCache(users),
	}
}

func (p StaticProvider) Type() enums.ProviderSource {
	return enums.ProviderSourceStatic
}

func (p StaticProvider) SupportsGroupPermissions() bool {
	return false
}

func (p *StaticProvider) GetByUsername(ctx context.Context, username string) (*User, error) {
	if err := tenant.RequireDefault(ctx); err != nil {
		return nil, err
	}
	return p.UserCache.GetByUsername(ctx, username)
}

func (p *StaticProvider) GetAll(ctx context.Context) ([]*User, error) {
	if err := tenant.RequireDefault(ctx); err != nil {
		return nil, err
	}
	return p.UserCache.GetAll(ctx)
}

func (p *StaticProvider) ListGroups(ctx context.Context) ([]Group, error) {
	if err := tenant.RequireDefault(ctx); err != nil {
		return nil, err
	}
	return nil, errors2.APIError{
		Message:    "The single user authentication doesn't support this feature.",
		HTTPStatus: http.StatusBadRequest,
	}
}

func (p *StaticProvider) GetGroup(ctx context.Context, _ string) (Group, error) {
	if err := tenant.RequireDefault(ctx); err != nil {
		return Group{}, err
	}
	return Group{}, errors2.APIError{
		Message:    "The single user authentication doesn't support this feature.",
		HTTPStatus: http.StatusBadRequest,
	}
}

func (p *StaticProvider) UpdateGroup(ctx context.Context, _ string, _ Group) error {
	if err := tenant.RequireDefault(ctx); err != nil {
		return err
	}
	return errors2.APIError{
		Message:    "The single user authentication doesn't support this feature.",
		HTTPStatus: http.StatusBadRequest,
	}
}

func (p *StaticProvider) DeleteGroup(ctx context.Context, _ string) error {
	if err := tenant.RequireDefault(ctx); err != nil {
		return err
	}
	return errors2.APIError{
		Message:    "The single user authentication doesn't support this feature.",
		HTTPStatus: http.StatusBadRequest,
	}
}

func (p *StaticProvider) Add(ctx context.Context, usr *User) error {
	if err := tenant.RequireDefault(ctx); err != nil {
		return err
	}
	return errors2.APIError{
		Message:    "The single user authentication doesn't support this operation.",
		HTTPStatus: http.StatusBadRequest,
	}
}
func (p *StaticProvider) Update(ctx context.Context, usr *User, username string) error {
	if err := tenant.RequireDefault(ctx); err != nil {
		return err
	}
	return errors2.APIError{
		Message:    "The single user authentication doesn't support this operation.",
		HTTPStatus: http.StatusBadRequest,
	}
}
func (p *StaticProvider) Delete(ctx context.Context, username string) error {
	if err := tenant.RequireDefault(ctx); err != nil {
		return err
	}
	return errors2.APIError{
		Message:    "The single user authentication doesn't support this operation.",
		HTTPStatus: http.StatusBadRequest,
	}
}
