package targeting

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	errors2 "github.com/pohcee/procy-server/server/api/errors"
	"github.com/pohcee/procy-server/server/cgroups"
	"github.com/pohcee/procy-server/server/clients/clientdata"
	"github.com/pohcee/procy-server/share/models"
)

type fakeClients map[string]*clientdata.Client

func (f fakeClients) GetByID(_ context.Context, id string) (*clientdata.Client, error) {
	return f[id], nil
}

func (f fakeClients) GetByGroups(_ context.Context, groups []*cgroups.ClientGroup) ([]*clientdata.Client, error) {
	var out []*clientdata.Client
	for _, g := range groups {
		for _, id := range g.ClientIDs {
			out = append(out, f[id])
		}
	}
	return out, nil
}

func (f fakeClients) GetClientsByTag(context.Context, []string, string, bool) ([]*clientdata.Client, error) {
	return []*clientdata.Client{f["c3"]}, nil
}

type fakeGroups map[string]*cgroups.ClientGroup

func (f fakeGroups) Get(_ context.Context, id string) (*cgroups.ClientGroup, error) {
	return f[id], nil
}

type params struct {
	ids, groups []string
	tags        *models.JobClientTags
}

func (p params) GetClientIDs() []string               { return p.ids }
func (p params) GetGroupIDs() []string                { return p.groups }
func (p params) GetClientTags() *models.JobClientTags { return p.tags }

func resolver() Resolver {
	cl := fakeClients{}
	for _, id := range []string{"c1", "c2", "c3"} {
		cl[id] = &clientdata.Client{ID: id}
	}
	return Resolver{Clients: cl, Groups: fakeGroups{"g1": {ID: "g1", ClientIDs: []string{"c2", "c1"}}}}
}

func ids(cls []*clientdata.Client) []string {
	out := make([]string, len(cls))
	for i, c := range cls {
		out[i] = c.GetID()
	}
	return out
}

func TestResolveOrdersClientIDsThenGroupsWithoutDuplicates(t *testing.T) {
	got, inGroups, err := resolver().Resolve(context.Background(), params{ids: []string{"c1"}, groups: []string{"g1"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"c1", "c2"}, ids(got))
	assert.Equal(t, 2, inGroups)
}

func TestResolveByTags(t *testing.T) {
	got, _, err := resolver().Resolve(context.Background(), params{tags: &models.JobClientTags{Tags: []string{"x"}}})
	require.NoError(t, err)
	assert.Equal(t, []string{"c3"}, ids(got))
}

func TestResolveErrors(t *testing.T) {
	cases := map[string]struct {
		p      params
		err    error
		status int
	}{
		"no targets":     {p: params{}, err: ErrRequestMissingTargetingParams, status: http.StatusBadRequest},
		"ids and tags":   {p: params{ids: []string{"c1"}, tags: &models.JobClientTags{Tags: []string{"x"}}}, err: ErrRequestIncludesMultipleTargetingParams, status: http.StatusBadRequest},
		"empty tags":     {p: params{tags: &models.JobClientTags{}}, err: ErrMissingTagsInMultiJobRequest, status: http.StatusBadRequest},
		"unknown client": {p: params{ids: []string{"nope"}}, status: http.StatusNotFound},
		"unknown group":  {p: params{groups: []string{"nope"}}, status: http.StatusBadRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := resolver().Resolve(context.Background(), tc.p)
			var apiErr errors2.APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, tc.status, apiErr.HTTPStatus)
			if tc.err != nil {
				assert.Equal(t, tc.err, apiErr.Err)
			}
		})
	}
}
