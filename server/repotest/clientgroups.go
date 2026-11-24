package repotest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/cgroups"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
)

// ClientGroups checks a cgroups.ClientGroupProvider.
func ClientGroups(t *testing.T, newRepo Factory[cgroups.ClientGroupProvider]) {
	a, b := Ctx(TenantA), Ctx(TenantB)

	t.Run("requires tenant", func(t *testing.T) {
		p := newRepo(t)
		ctx := context.Background()
		_, err := p.Get(ctx, "g1")
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = p.GetAll(ctx)
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = p.List(ctx, &query.ListOptions{})
		assert.ErrorIs(t, err, tenant.ErrMissing)
		assert.ErrorIs(t, p.Create(ctx, clientGroup("g1", "")), tenant.ErrMissing)
		assert.ErrorIs(t, p.Update(ctx, clientGroup("g1", "")), tenant.ErrMissing)
		assert.ErrorIs(t, p.Delete(ctx, "g1"), tenant.ErrMissing)
	})

	t.Run("round trip", func(t *testing.T) {
		p := newRepo(t)
		tag := json.RawMessage(`{"and":["prod","eu"]}`)
		g := &cgroups.ClientGroup{
			ID:          "g1",
			Description: "full group",
			Params: &cgroups.ClientParams{
				ClientID:        &cgroups.ParamValues{"c1", "c2*"},
				Name:            &cgroups.ParamValues{"web*"},
				OS:              &cgroups.ParamValues{"linux"},
				OSArch:          &cgroups.ParamValues{"amd64"},
				OSFamily:        &cgroups.ParamValues{"debian"},
				OSKernel:        &cgroups.ParamValues{"linux"},
				Hostname:        &cgroups.ParamValues{"h1"},
				IPv4:            &cgroups.ParamValues{"192.0.2.*"},
				IPv6:            &cgroups.ParamValues{"2001:db8::1"},
				Tag:             &tag,
				Version:         &cgroups.ParamValues{"1.*"},
				Address:         &cgroups.ParamValues{"198.51.100.7"},
				ClientAuthID:    &cgroups.ParamValues{"auth"},
				ConnectionState: &cgroups.ParamValues{"connected"},
			},
			AllowedUserGroups: []string{"admins", "ops"},
		}
		require.NoError(t, p.Create(a, g))
		assert.Equal(t, TenantA, g.TenantID)

		got, err := p.Get(a, "g1")
		require.NoError(t, err)
		want := *g
		want.TenantID = TenantA
		assert.Equal(t, &want, got)

		// an empty group keeps its empty values
		require.NoError(t, p.Create(a, clientGroup("empty", "")))
		got, err = p.Get(a, "empty")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, &cgroups.ClientParams{}, got.Params)
		assert.NotNil(t, got.AllowedUserGroups)
		assert.Empty(t, got.AllowedUserGroups)
	})

	t.Run("get unknown", func(t *testing.T) {
		p := newRepo(t)
		got, err := p.Get(a, "unknown")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("create refuses an existing id", func(t *testing.T) {
		p := newRepo(t)
		require.NoError(t, p.Create(a, clientGroup("g1", "first")))
		assert.Error(t, p.Create(a, clientGroup("g1", "second")))
		got, err := p.Get(a, "g1")
		require.NoError(t, err)
		assert.Equal(t, "first", got.Description)
	})

	t.Run("update replaces or creates", func(t *testing.T) {
		p := newRepo(t)
		require.NoError(t, p.Create(a, clientGroup("g1", "first")))
		changed := clientGroup("g1", "changed")
		changed.Params.OS = &cgroups.ParamValues{"windows"}
		changed.AllowedUserGroups = []string{"ops"}
		require.NoError(t, p.Update(a, changed))
		got, err := p.Get(a, "g1")
		require.NoError(t, err)
		assert.Equal(t, "changed", got.Description)
		assert.Equal(t, &cgroups.ParamValues{"windows"}, got.Params.OS)
		assert.Equal(t, []string{"ops"}, []string(got.AllowedUserGroups))

		require.NoError(t, p.Update(a, clientGroup("new", "made by update")))
		got, err = p.Get(a, "new")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "made by update", got.Description)
	})

	t.Run("delete", func(t *testing.T) {
		p := newRepo(t)
		require.NoError(t, p.Create(a, clientGroup("g1", "")))
		require.NoError(t, p.Create(a, clientGroup("g2", "")))
		require.NoError(t, p.Delete(a, "g1"))
		require.NoError(t, p.Delete(a, "unknown"))
		all, err := p.GetAll(a)
		require.NoError(t, err)
		assert.Equal(t, []string{"g2"}, groupIDs(all))
	})

	t.Run("get all sorts by id ignoring case", func(t *testing.T) {
		p := newRepo(t)
		for _, id := range []string{"b", "C", "a", "D"} {
			require.NoError(t, p.Create(a, clientGroup(id, "")))
		}
		all, err := p.GetAll(a)
		require.NoError(t, err)
		assert.Equal(t, []string{"a", "b", "C", "D"}, groupIDs(all))
		assert.Equal(t, TenantA, all[0].TenantID)

		empty, err := p.GetAll(b)
		require.NoError(t, err)
		assert.Empty(t, empty)
	})

	t.Run("list options", func(t *testing.T) {
		p := newRepo(t)
		for _, g := range []*cgroups.ClientGroup{
			clientGroup("linux", "Linux servers"),
			clientGroup("windows", "Windows desktops"),
			clientGroup("web", "linux web servers"),
		} {
			require.NoError(t, p.Create(a, g))
		}
		eq := func(col string, values ...string) query.FilterOption {
			return query.FilterOption{Column: []string{col}, Operator: query.FilterOperatorTypeEQ, Values: values}
		}
		cases := []struct {
			name string
			opts *query.ListOptions
			want []string
		}{
			{"no options", &query.ListOptions{}, nil},
			{"id", &query.ListOptions{Filters: []query.FilterOption{eq("id", "web")}}, []string{"web"}},
			{"id one of", &query.ListOptions{Filters: []query.FilterOption{eq("id", "web", "linux")}}, []string{"linux", "web"}},
			{"description wildcard ignores case", &query.ListOptions{Filters: []query.FilterOption{eq("description", "*linux*")}}, []string{"linux", "web"}},
			{"id or description", &query.ListOptions{Filters: []query.FilterOption{{
				Column: []string{"id", "description"}, Operator: query.FilterOperatorTypeEQ, Values: []string{"windows*"},
			}}}, []string{"windows"}},
			{"sort by id", &query.ListOptions{Sorts: []query.SortOption{{Column: "id", IsASC: true}}}, []string{"linux", "web", "windows"}},
			{"sort by description desc", &query.ListOptions{Sorts: []query.SortOption{{Column: "description"}}}, []string{"web", "windows", "linux"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := p.List(a, tc.opts)
				require.NoError(t, err)
				ids := groupIDs(got)
				switch {
				case tc.want == nil:
					assert.ElementsMatch(t, []string{"linux", "web", "windows"}, ids)
				case len(tc.opts.Sorts) == 0:
					assert.ElementsMatch(t, tc.want, ids)
				default:
					assert.Equal(t, tc.want, ids)
				}
			})
		}
	})

	t.Run("isolates tenants", func(t *testing.T) {
		p := newRepo(t)
		require.NoError(t, p.Create(a, clientGroup("g1", "from a")))
		require.NoError(t, p.Create(b, clientGroup("g1", "from b")))

		got, err := p.Get(a, "g1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "from a", got.Description)

		all, err := p.GetAll(a)
		require.NoError(t, err)
		assert.Len(t, all, 1)

		listed, err := p.List(b, &query.ListOptions{Filters: []query.FilterOption{{
			Column: []string{"id"}, Operator: query.FilterOperatorTypeEQ, Values: []string{"g1"},
		}}})
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, "from b", listed[0].Description)
		assert.Equal(t, TenantB, listed[0].TenantID)

		require.NoError(t, p.Update(a, clientGroup("g1", "a updated")))
		still, err := p.Get(b, "g1")
		require.NoError(t, err)
		assert.Equal(t, "from b", still.Description)

		require.NoError(t, p.Delete(a, "g1"))
		gone, err := p.Get(a, "g1")
		require.NoError(t, err)
		assert.Nil(t, gone)
		still, err = p.Get(b, "g1")
		require.NoError(t, err)
		assert.NotNil(t, still)
	})

	t.Run("ignores the tenant on the struct", func(t *testing.T) {
		p := newRepo(t)
		g := clientGroup("g1", "")
		g.TenantID = TenantB
		require.NoError(t, p.Create(a, g))
		got, err := p.Get(b, "g1")
		require.NoError(t, err)
		assert.Nil(t, got)

		u := clientGroup("g2", "")
		u.TenantID = TenantB
		require.NoError(t, p.Update(a, u))
		got, err = p.Get(b, "g2")
		require.NoError(t, err)
		assert.Nil(t, got)
		got, err = p.Get(a, "g2")
		require.NoError(t, err)
		assert.NotNil(t, got)
	})
}

func clientGroup(id, desc string) *cgroups.ClientGroup {
	return &cgroups.ClientGroup{ID: id, Description: desc, Params: &cgroups.ClientParams{}, AllowedUserGroups: []string{}}
}

func groupIDs(gs []*cgroups.ClientGroup) []string {
	ids := make([]string, 0, len(gs))
	for _, g := range gs {
		ids = append(ids, g.ID)
	}
	return ids
}
