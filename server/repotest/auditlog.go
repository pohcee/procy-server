package repotest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/auditlog"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
)

var auditT0 = time.Date(2021, 10, 19, 13, 57, 58, 0, time.UTC)

func auditUsernames(t *testing.T, p auditlog.Provider, ctx context.Context, o *query.ListOptions) []string {
	t.Helper()
	if o == nil {
		o = &query.ListOptions{}
	}
	entries, err := p.List(ctx, o)
	require.NoError(t, err)
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Username)
	}
	return names
}

func auditFilter(col string, op query.FilterOperatorType, vals ...string) query.FilterOption {
	return query.FilterOption{Column: []string{col}, Operator: op, Values: vals}
}

// AuditLog runs the contract tests of auditlog.Provider.
func AuditLog(t *testing.T, newProvider Factory[auditlog.Provider]) {
	a, b := Ctx(TenantA), Ctx(TenantB)

	t.Run("RequiresTenant", func(t *testing.T) {
		p := newProvider(t)
		ctx := context.Background()
		assert.ErrorIs(t, p.Save(ctx, &auditlog.Entry{Timestamp: time.Now()}), tenant.ErrMissing)
		_, err := p.List(ctx, &query.ListOptions{})
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = p.Count(ctx, &query.ListOptions{})
		assert.ErrorIs(t, err, tenant.ErrMissing)
	})

	t.Run("SaveRoundTrip", func(t *testing.T) {
		p := newProvider(t)
		e := &auditlog.Entry{
			Timestamp:      auditT0,
			Username:       "admin",
			RemoteIP:       "192.168.55.23",
			Application:    auditlog.ApplicationLibraryCommand,
			Action:         auditlog.ActionCreate,
			ID:             "db4960c8-c7dd-42b8-9db6-2a98dc7122d9",
			ClientID:       "e9e7e70c-d023-4423-869c-86d70da8f243",
			ClientHostName: "127.0.0.1",
			Request:        `{"k1": "v1"}`,
			Response:       `{"k2": "v2"}`,
			TenantID:       TenantB, // the context's tenant wins
		}
		require.NoError(t, p.Save(a, e))
		assert.Equal(t, TenantA, e.TenantID, "Save stamps the context's tenant")

		entries, err := p.List(a, &query.ListOptions{})
		require.NoError(t, err)
		require.Len(t, entries, 1)
		got := entries[0]
		assert.True(t, auditT0.Equal(got.Timestamp), got.Timestamp)
		assert.Equal(t, "admin", got.Username)
		assert.Equal(t, "192.168.55.23", got.RemoteIP)
		assert.Equal(t, auditlog.ApplicationLibraryCommand, got.Application)
		assert.Equal(t, auditlog.ActionCreate, got.Action)
		assert.Equal(t, e.ID, got.ID)
		assert.Equal(t, e.ClientID, got.ClientID)
		assert.Equal(t, "127.0.0.1", got.ClientHostName)
		assert.Equal(t, `{"k1": "v1"}`, got.Request)
		assert.Equal(t, `{"k2": "v2"}`, got.Response)
		assert.Equal(t, TenantA, got.TenantID)

		assert.Empty(t, auditUsernames(t, p, b, nil))
	})

	t.Run("EmptyFields", func(t *testing.T) {
		p := newProvider(t)
		require.NoError(t, p.Save(a, &auditlog.Entry{Timestamp: auditT0, Application: "client", Action: "create"}))
		entries, err := p.List(a, &query.ListOptions{})
		require.NoError(t, err)
		require.Len(t, entries, 1)
		assert.Equal(t, "", entries[0].Username)
		assert.Equal(t, "", entries[0].Request)
	})

	t.Run("IsolatesTenants", func(t *testing.T) {
		p := newProvider(t)
		require.NoError(t, p.Save(a, &auditlog.Entry{Timestamp: time.Now(), Username: "alice", ID: "x"}))
		require.NoError(t, p.Save(b, &auditlog.Entry{Timestamp: time.Now(), Username: "bob", ID: "x"}))
		require.NoError(t, p.Save(b, &auditlog.Entry{Timestamp: time.Now(), Username: "bob2", ID: "y"}))

		assert.Equal(t, []string{"alice"}, auditUsernames(t, p, a, nil))
		assert.ElementsMatch(t, []string{"bob", "bob2"}, auditUsernames(t, p, b, nil))

		filtered, err := p.List(b, &query.ListOptions{Filters: []query.FilterOption{auditFilter("affected_id", query.FilterOperatorTypeEQ, "x")}})
		require.NoError(t, err)
		require.Len(t, filtered, 1)
		assert.Equal(t, "bob", filtered[0].Username)

		count, err := p.Count(a, &query.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, 1, count)
		count, err = p.Count(b, &query.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, 2, count)
		count, err = p.Count(Ctx("c"), &query.ListOptions{})
		require.NoError(t, err)
		assert.Zero(t, count)
	})

	t.Run("FiltersSortsAndPagination", func(t *testing.T) {
		p := newProvider(t)
		for i, e := range []auditlog.Entry{
			{Username: "carol", Application: auditlog.ApplicationLibraryScript, Action: auditlog.ActionCreate, ID: "s1", ClientID: "c1", ClientHostName: "h1", RemoteIP: "10.0.0.1"},
			{Username: "alice", Application: auditlog.ApplicationLibraryScript, Action: auditlog.ActionUpdate, ID: "s1", ClientID: "c2", ClientHostName: "h2", RemoteIP: "10.0.0.2"},
			{Username: "bob", Application: auditlog.ApplicationLibraryCommand, Action: auditlog.ActionCreate, ID: "c9", ClientID: "c1", ClientHostName: "h1", RemoteIP: "10.0.0.3"},
			{Username: "alex", Application: auditlog.ApplicationClient, Action: auditlog.ActionDelete, ID: "x", ClientID: "c3", ClientHostName: "h3", RemoteIP: "10.0.0.4"},
		} {
			e := e
			e.Timestamp = auditT0.Add(time.Duration(i) * 24 * time.Hour)
			require.NoError(t, p.Save(a, &e))
		}
		opts := func(sort string, asc bool, filters ...query.FilterOption) *query.ListOptions {
			o := &query.ListOptions{Filters: filters}
			if sort != "" {
				o.Sorts = []query.SortOption{{Column: sort, IsASC: asc}}
			}
			return o
		}

		assert.Equal(t, []string{"alex", "alice", "bob", "carol"}, auditUsernames(t, p, a, opts("username", true)))
		assert.Equal(t, []string{"alex", "bob", "alice", "carol"}, auditUsernames(t, p, a, opts("timestamp", false)))
		assert.Equal(t, []string{"carol", "alice", "bob", "alex"}, auditUsernames(t, p, a, opts("timestamp", true)))
		assert.Equal(t, []string{"bob", "carol"}, auditUsernames(t, p, a, opts("remote_ip", false,
			auditFilter("remote_ip", query.FilterOperatorTypeEQ, "10.0.0.1", "10.0.0.3"))))
		for _, col := range []string{"application", "action", "affected_id", "client_id", "client_hostname"} {
			assert.Len(t, auditUsernames(t, p, a, opts(col, true)), 4, "sort by "+col)
		}

		for _, tc := range []struct {
			filter query.FilterOption
			want   []string
		}{
			{auditFilter("application", query.FilterOperatorTypeEQ, auditlog.ApplicationLibraryScript), []string{"alice", "carol"}},
			{auditFilter("action", query.FilterOperatorTypeEQ, auditlog.ActionCreate), []string{"bob", "carol"}},
			{auditFilter("affected_id", query.FilterOperatorTypeEQ, "s1"), []string{"alice", "carol"}},
			{auditFilter("client_id", query.FilterOperatorTypeEQ, "c1"), []string{"bob", "carol"}},
			{auditFilter("client_hostname", query.FilterOperatorTypeEQ, "h3"), []string{"alex"}},
			{auditFilter("remote_ip", query.FilterOperatorTypeEQ, "10.0.0.2"), []string{"alice"}},
			{auditFilter("username", query.FilterOperatorTypeEQ, "al*"), []string{"alex", "alice"}},
			{auditFilter("username", query.FilterOperatorTypeEQ, "bob", "carol"), []string{"bob", "carol"}},
			{auditFilter("timestamp", query.FilterOperatorTypeGT, auditT0.Add(25*time.Hour).Format("2006-01-02 15:04:05")), []string{"alex", "bob"}},
			{auditFilter("timestamp", query.FilterOperatorTypeLT, auditT0.Add(23*time.Hour).Format("2006-01-02 15:04:05")), []string{"carol"}},
			{auditFilter("timestamp", query.FilterOperatorTypeSince, auditT0.Add(2*24*time.Hour).Format("2006-01-02")), []string{"alex", "bob"}},
			{auditFilter("timestamp", query.FilterOperatorTypeUntil, auditT0.Add(24*time.Hour).Format("2006-01-02")), []string{"carol"}},
		} {
			assert.Equal(t, tc.want, auditUsernames(t, p, a, opts("username", true, tc.filter)), tc.filter.String())
			count, err := p.Count(a, opts("", false, tc.filter))
			require.NoError(t, err)
			assert.Equal(t, len(tc.want), count, tc.filter.String())
		}

		// two filters are ANDed
		assert.Equal(t, []string{"carol"}, auditUsernames(t, p, a, opts("username", true,
			auditFilter("application", query.FilterOperatorTypeEQ, auditlog.ApplicationLibraryScript),
			auditFilter("action", query.FilterOperatorTypeEQ, auditlog.ActionCreate))))

		// as the API asks: second page of one entry, newest first
		paged := opts("timestamp", false, auditFilter("application", query.FilterOperatorTypeEQ, auditlog.ApplicationLibraryScript))
		paged.Pagination = &query.Pagination{Limit: "1", Offset: "1"}
		assert.Equal(t, []string{"carol"}, auditUsernames(t, p, a, paged))
		count, err := p.Count(a, paged)
		require.NoError(t, err)
		assert.Equal(t, 2, count, "count ignores pagination")
	})
}
