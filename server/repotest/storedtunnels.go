package repotest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clients/storedtunnels"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
	"github.com/openrport/openrport/share/types"
)

// StoredTunnelRepos is a stored tunnel provider and the client store on the same storage, which holds the clients
// the tunnels belong to (stores may require the client to exist).
type StoredTunnelRepos struct {
	Clients clients.ClientStore
	Tunnels storedtunnels.Provider
}

// StoredTunnels checks a storedtunnels.Provider.
func StoredTunnels(t *testing.T, newRepos Factory[StoredTunnelRepos]) {
	a, b := Ctx(TenantA), Ctx(TenantB)
	all := &query.ListOptions{}

	// setup returns the provider with clients c1 and c2 in both tenants.
	setup := func(t *testing.T) storedtunnels.Provider {
		r := newRepos(t)
		for _, ctx := range []context.Context{a, b} {
			for _, id := range []string{"c1", "c2"} {
				require.NoError(t, r.Clients.Save(ctx, StoreClient(id, nil)))
			}
		}
		return r.Tunnels
	}

	t.Run("requires tenant", func(t *testing.T) {
		p := setup(t)
		ctx := context.Background()
		assert.ErrorIs(t, p.Insert(ctx, tunnel("t1", "c1", "web")), tenant.ErrMissing)
		assert.ErrorIs(t, p.Update(ctx, tunnel("t1", "c1", "web")), tenant.ErrMissing)
		_, err := p.List(ctx, "c1", all)
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = p.Count(ctx, "c1", all)
		assert.ErrorIs(t, err, tenant.ErrMissing)
		assert.ErrorIs(t, p.Delete(ctx, "c1", "t1"), tenant.ErrMissing)
	})

	t.Run("round trip", func(t *testing.T) {
		p := setup(t)
		full := tunnel("t1", "c1", "web")
		full.FurtherOptions = jsonString(`{"http_proxy":true,"host_header":"example.com"}`)
		empty := &storedtunnels.StoredTunnel{ID: "t2", ClientID: "c1", CreatedAt: full.CreatedAt.Add(time.Second)}
		require.NoError(t, p.Insert(a, full))
		require.NoError(t, p.Insert(a, empty))

		got, err := p.List(a, "c1", &query.ListOptions{Sorts: []query.SortOption{{Column: "created_at", IsASC: true}}})
		require.NoError(t, err)
		require.Len(t, got, 2)
		assertSameTunnel(t, full, got[0])
		assertSameTunnel(t, empty, got[1])
		assert.Equal(t, TenantA, got[0].TenantID)

		n, err := p.Count(a, "c1", all)
		require.NoError(t, err)
		assert.Equal(t, 2, n)
	})

	t.Run("empty list", func(t *testing.T) {
		p := setup(t)
		got, err := p.List(a, "c1", all)
		require.NoError(t, err)
		assert.Empty(t, got)
		n, err := p.Count(a, "c1", all)
		require.NoError(t, err)
		assert.Zero(t, n)
	})

	t.Run("insert ignores the tenant on the struct", func(t *testing.T) {
		p := setup(t)
		tu := tunnel("t1", "c1", "web")
		tu.TenantID = TenantB
		require.NoError(t, p.Insert(a, tu))
		assert.Equal(t, TenantA, tu.TenantID)
		assert.Len(t, list(a, t, p, "c1", all), 1)
		assert.Empty(t, list(b, t, p, "c1", all))
	})

	t.Run("tunnels are listed per client", func(t *testing.T) {
		p := setup(t)
		require.NoError(t, p.Insert(a, tunnel("t1", "c1", "one")))
		require.NoError(t, p.Insert(a, tunnel("t2", "c2", "two")))
		got := list(a, t, p, "c2", all)
		require.Len(t, got, 1)
		assert.Equal(t, "t2", got[0].ID)
		assert.Empty(t, list(a, t, p, "unknown", all))
	})

	t.Run("update", func(t *testing.T) {
		p := setup(t)
		orig := tunnel("t1", "c1", "web")
		require.NoError(t, p.Insert(a, orig))
		changed := tunnel("t1", "c1", "renamed")
		changed.CreatedAt = orig.CreatedAt.Add(time.Hour) // not updated
		changed.Scheme, changed.RemoteIP, changed.RemotePort = strPtr("rdp"), strPtr("10.1.1.1"), intPtr(3389)
		changed.PublicPort, changed.ACL = intPtr(23389), nil
		changed.FurtherOptions = jsonString(`{"idle_timeout_minutes":7}`)
		require.NoError(t, p.Update(a, changed))

		got := list(a, t, p, "c1", all)
		require.Len(t, got, 1)
		changed.CreatedAt = orig.CreatedAt
		assertSameTunnel(t, changed, got[0])

		// the tunnel is found by client and id
		other := tunnel("t1", "c2", "wrong client")
		require.NoError(t, p.Update(a, other))
		assert.Equal(t, "renamed", list(a, t, p, "c1", all)[0].Name)
		assert.Empty(t, list(a, t, p, "c2", all))
	})

	t.Run("delete", func(t *testing.T) {
		p := setup(t)
		require.NoError(t, p.Insert(a, tunnel("t1", "c1", "one")))
		require.NoError(t, p.Insert(a, tunnel("t2", "c1", "two")))
		require.NoError(t, p.Delete(a, "c2", "t1")) // other client: nothing deleted
		require.NoError(t, p.Delete(a, "c1", "unknown"))
		require.NoError(t, p.Delete(a, "c1", "t1"))
		got := list(a, t, p, "c1", all)
		require.Len(t, got, 1)
		assert.Equal(t, "t2", got[0].ID)
	})

	t.Run("list options", func(t *testing.T) {
		p := setup(t)
		base := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
		for i, tu := range []*storedtunnels.StoredTunnel{
			{ID: "t1", Name: "web server", Scheme: strPtr("http"), RemoteIP: strPtr("127.0.0.1"), RemotePort: intPtr(80)},
			{ID: "t2", Name: "Webmail", Scheme: strPtr("https"), RemoteIP: strPtr("127.0.0.1"), RemotePort: intPtr(443)},
			{ID: "t3", Name: "ssh", Scheme: strPtr("ssh"), RemoteIP: strPtr("10.0.0.2"), RemotePort: intPtr(22)},
			{ID: "t4", Name: "rdp", Scheme: strPtr("rdp"), RemoteIP: strPtr("10.0.0.3"), RemotePort: intPtr(3389)},
		} {
			tu.ClientID = "c1"
			tu.CreatedAt = base.Add(time.Duration(i) * time.Minute)
			require.NoError(t, p.Insert(a, tu))
		}
		eq := func(col string, values ...string) query.FilterOption {
			return query.FilterOption{Column: []string{col}, Operator: query.FilterOperatorTypeEQ, Values: values}
		}
		cases := []struct {
			name string
			opts *query.ListOptions
			want []string
		}{
			{"name", &query.ListOptions{Filters: []query.FilterOption{eq("name", "ssh")}}, []string{"t3"}},
			{"name wildcard ignores case", &query.ListOptions{Filters: []query.FilterOption{eq("name", "web*")}}, []string{"t1", "t2"}},
			{"scheme one of", &query.ListOptions{Filters: []query.FilterOption{eq("scheme", "ssh", "rdp")}}, []string{"t3", "t4"}},
			{"remote ip", &query.ListOptions{Filters: []query.FilterOption{eq("remote_ip", "127.0.0.1")}}, []string{"t1", "t2"}},
			{"remote port", &query.ListOptions{Filters: []query.FilterOption{eq("remote_port", "22")}}, []string{"t3"}},
			{"two filters", &query.ListOptions{Filters: []query.FilterOption{eq("remote_ip", "127.0.0.1"), eq("scheme", "https")}}, []string{"t2"}},
			{"sort by created_at desc", &query.ListOptions{Sorts: []query.SortOption{{Column: "created_at"}}}, []string{"t4", "t3", "t2", "t1"}},
			{"sort by name", &query.ListOptions{Sorts: []query.SortOption{{Column: "name", IsASC: true}}}, []string{"t2", "t4", "t3", "t1"}},
			{"sort by remote_port", &query.ListOptions{Sorts: []query.SortOption{{Column: "remote_port", IsASC: true}}}, []string{"t3", "t1", "t2", "t4"}},
			{"sort by remote_ip desc, scheme", &query.ListOptions{Sorts: []query.SortOption{{Column: "remote_ip", IsASC: false}, {Column: "scheme", IsASC: true}}}, []string{"t1", "t2", "t4", "t3"}},
			{"pagination", &query.ListOptions{
				Sorts:      []query.SortOption{{Column: "created_at", IsASC: true}},
				Pagination: query.NewPagination(2, 1),
			}, []string{"t2", "t3"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := list(a, t, p, "c1", tc.opts)
				ids := make([]string, 0, len(got))
				for _, tu := range got {
					ids = append(ids, tu.ID)
				}
				if len(tc.opts.Sorts) == 0 {
					assert.ElementsMatch(t, tc.want, ids)
				} else {
					assert.Equal(t, tc.want, ids)
				}
			})
		}

		// count applies filters and ignores pagination
		n, err := p.Count(a, "c1", &query.ListOptions{
			Filters:    []query.FilterOption{eq("remote_ip", "127.0.0.1", "10.0.0.2")},
			Pagination: query.NewPagination(1, 0),
		})
		require.NoError(t, err)
		assert.Equal(t, 3, n)
	})

	t.Run("isolates tenants", func(t *testing.T) {
		p := setup(t)
		require.NoError(t, p.Insert(a, tunnel("same", "c1", "from a")))
		require.NoError(t, p.Insert(b, tunnel("same", "c1", "from b")))

		got := list(b, t, p, "c1", all)
		require.Len(t, got, 1)
		assert.Equal(t, "from b", got[0].Name)
		assert.Equal(t, TenantB, got[0].TenantID)
		n, err := p.Count(a, "c1", all)
		require.NoError(t, err)
		assert.Equal(t, 1, n)

		require.NoError(t, p.Update(a, tunnel("same", "c1", "a updated")))
		assert.Equal(t, "from b", list(b, t, p, "c1", all)[0].Name)
		assert.Equal(t, "a updated", list(a, t, p, "c1", all)[0].Name)

		// a tunnel only b has can't be changed from a
		require.NoError(t, p.Insert(b, tunnel("only-b", "c1", "b only")))
		require.NoError(t, p.Update(a, tunnel("only-b", "c1", "changed by a")))
		require.NoError(t, p.Delete(a, "c1", "only-b"))
		assert.Len(t, list(b, t, p, "c1", all), 2)
		assert.Len(t, list(a, t, p, "c1", all), 1)

		require.NoError(t, p.Delete(a, "c1", "same"))
		assert.Empty(t, list(a, t, p, "c1", all))
		got = list(b, t, p, "c1", all)
		require.Len(t, got, 2)
		for _, tu := range got {
			assert.NotEqual(t, "changed by a", tu.Name)
		}
	})
}

func list(ctx context.Context, t *testing.T, p storedtunnels.Provider, clientID string, o *query.ListOptions) []*storedtunnels.StoredTunnel {
	t.Helper()
	got, err := p.List(ctx, clientID, o)
	require.NoError(t, err)
	return got
}

func tunnel(id, clientID, name string) *storedtunnels.StoredTunnel {
	return &storedtunnels.StoredTunnel{
		ID:         id,
		ClientID:   clientID,
		CreatedAt:  time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC),
		Name:       name,
		Scheme:     strPtr("ssh"),
		RemoteIP:   strPtr("127.0.0.1"),
		RemotePort: intPtr(22),
		PublicPort: intPtr(20022),
		ACL:        strPtr("192.0.2.0/24"),
	}
}

func assertSameTunnel(t *testing.T, want, got *storedtunnels.StoredTunnel) {
	t.Helper()
	assert.True(t, want.CreatedAt.Equal(got.CreatedAt), "created at: want %s, got %s", want.CreatedAt, got.CreatedAt)
	w, g := *want, *got
	w.TenantID, g.TenantID = "", ""
	w.CreatedAt, g.CreatedAt = time.Time{}, time.Time{}
	if w.FurtherOptions != nil && g.FurtherOptions != nil {
		assert.JSONEq(t, string(*w.FurtherOptions), string(*g.FurtherOptions))
		w.FurtherOptions, g.FurtherOptions = nil, nil
	}
	assert.Equal(t, w, g)
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }
func jsonString(s string) *types.JSONString {
	js := types.JSONString(s)
	return &js
}
