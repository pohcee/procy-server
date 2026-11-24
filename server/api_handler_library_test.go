package chserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	librarymigration "github.com/openrport/openrport/db/migration/library"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/share/tenant"
)

func defaultTenantRequest(method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	return req.WithContext(tenant.WithID(req.Context(), tenant.Default))
}

func newLibraryViewListener(t *testing.T) *APIListener {
	t.Helper()
	db, err := sqlite.New(filepath.Join(t.TempDir(), "library.db"), librarymigration.AssetNames(), librarymigration.Asset, sqlite.DataSourceOptions{WALEnabled: false})
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	store := library.NewStore(db)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := library.NewManager(store, func() time.Time { now = now.Add(time.Minute); return now })
	ctx := tenant.WithID(context.Background(), tenant.Default)
	for _, c := range []library.Content{
		{Name: "b-script", Tags: []string{"x"}, Implementations: map[string]library.Implementation{library.OSLinux: {Kind: library.KindScript, Code: "echo b", Interpreter: "bash"}}},
		{Name: "a-script", Implementations: map[string]library.Implementation{library.OSLinux: {Kind: library.KindScript, Code: "echo a"}}},
		{Name: "cmd", Implementations: map[string]library.Implementation{library.OSLinux: {Kind: library.KindCommand, Code: "/bin/ls"}}},
	} {
		_, err := m.Create(ctx, c, "m", "u")
		require.NoError(t, err)
	}
	return &APIListener{libraryManager: m}
}

func TestListLibraryViewFilterSortFieldsPaginate(t *testing.T) {
	al := newLibraryViewListener(t)
	list := func(rawQuery string) ([]string, int) {
		req := defaultTenantRequest(http.MethodGet, "/library/scripts?"+rawQuery)
		items, total, err := listLibraryView(al, req, library.KindScript, "scripts", scriptSortAndFilters, scriptFields,
			library.ToScript, library.PruneScript)
		require.NoError(t, err)
		names := []string{}
		for _, s := range items {
			names = append(names, s.Name)
		}
		return names, total
	}

	names, total := list("sort=name")
	assert.Equal(t, []string{"a-script", "b-script"}, names)
	assert.Equal(t, 2, total)

	names, _ = list("sort=-name")
	assert.Equal(t, []string{"b-script", "a-script"}, names)

	names, _ = list("filter[tags]=x")
	assert.Equal(t, []string{"b-script"}, names)

	names, total = list("sort=name&page[limit]=1&page[offset]=1")
	assert.Equal(t, []string{"b-script"}, names)
	assert.Equal(t, 2, total)

	req := defaultTenantRequest(http.MethodGet, "/library/scripts?fields[scripts]=id")
	items, _, err := listLibraryView(al, req, library.KindScript, "scripts", scriptSortAndFilters, scriptFields,
		library.ToScript, library.PruneScript)
	require.NoError(t, err)
	require.NotEmpty(t, items)
	assert.NotEmpty(t, items[0].ID)
	assert.Empty(t, items[0].Name)
	assert.Nil(t, items[0].Tags)

	_, _, err = listLibraryView(al, defaultTenantRequest(http.MethodGet, "/library/scripts?sort=bogus"),
		library.KindScript, "scripts", scriptSortAndFilters, scriptFields, library.ToScript, library.PruneScript)
	assert.Error(t, err)
}
