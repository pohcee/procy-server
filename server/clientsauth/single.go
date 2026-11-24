package clientsauth

import (
	"context"
	"errors"

	"github.com/openrport/openrport/share/enums"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
)

type SingleProvider struct {
	client *ClientAuth
}

var _ Provider = &SingleProvider{}

func NewSingleProvider(id, password string) *SingleProvider {
	return &SingleProvider{
		client: &ClientAuth{
			ID:       id,
			Password: password,
		},
	}
}

func (c *SingleProvider) GetFiltered(ctx context.Context, filter *query.ListOptions) ([]*ClientAuth, int, error) {
	if err := tenant.RequireDefault(ctx); err != nil {
		return nil, 0, err
	}
	var ca = []*ClientAuth{c.client}
	if len(filter.Filters) > 0 {
		match, err := query.MatchesFilters(ca[0], filter.Filters)
		if err != nil {
			return nil, 0, err
		}
		if match {
			return ca, 1, nil
		}
		return []*ClientAuth{}, 0, nil
	}
	start, _ := filter.Pagination.GetStartEnd(1)
	if start > 0 {
		return []*ClientAuth{}, 1, nil
	}
	return ca, 1, nil
}

func (c *SingleProvider) Get(ctx context.Context, id string) (*ClientAuth, error) {
	if err := tenant.RequireDefault(ctx); err != nil {
		return nil, err
	}
	if c.client.ID == id {
		return c.client, nil
	}
	return nil, nil
}

func (c *SingleProvider) Add(ctx context.Context, _ *ClientAuth) (bool, error) {
	if err := tenant.RequireDefault(ctx); err != nil {
		return false, err
	}
	return false, errors.New("not implemented")
}

func (c *SingleProvider) Delete(ctx context.Context, _ string) error {
	if err := tenant.RequireDefault(ctx); err != nil {
		return err
	}
	return errors.New("not implemented")
}

func (c *SingleProvider) IsWriteable() bool {
	return false
}

func (c *SingleProvider) Source() enums.ProviderSource {
	return enums.ProviderSourceStatic
}
