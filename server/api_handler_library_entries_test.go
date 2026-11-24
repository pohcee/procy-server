package chserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	librarymigration "github.com/openrport/openrport/db/migration/library"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/share/tenant"
)

type libraryEntriesEnv struct {
	router *mux.Router
	al     *APIListener
}

func newLibraryEntriesEnv(t *testing.T) *libraryEntriesEnv {
	t.Helper()
	db, err := sqlite.New(filepath.Join(t.TempDir(), "library.db"), librarymigration.FS, sqlite.DataSourceOptions{WALEnabled: false})
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := library.NewManager(library.NewSQLiteStore(db), func() time.Time { now = now.Add(time.Minute); return now })
	al := &APIListener{
		Server:           &Server{libraryManager: m},
		insecureForTests: true,
	}
	useLocalDispatcher(al)
	r := mux.NewRouter()
	al.addLibraryEntriesRoutes(r)
	return &libraryEntriesEnv{router: r, al: al}
}

func (e *libraryEntriesEnv) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req = req.WithContext(api.WithUser(tenant.WithID(req.Context(), tenant.Default), "admin"))
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

func (e *libraryEntriesEnv) data(t *testing.T, w *httptest.ResponseRecorder, into interface{}) {
	t.Helper()
	var p struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &p), w.Body.String())
	require.NoError(t, json.Unmarshal(p.Data, into), w.Body.String())
}

const entryBody = `{"name":"ls","message":"first","implementations":{"linux":{"kind":"command","code":"ls"}}}`

func TestLibraryEntriesLifecycle(t *testing.T) {
	e := newLibraryEntriesEnv(t)

	w := e.do(t, http.MethodPost, "/library/entries", entryBody)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var created library.Entry
	e.data(t, w, &created)
	assert.Equal(t, 1, created.ActiveVersion)
	id := created.ID
	require.NotEmpty(t, id)

	w = e.do(t, http.MethodGet, "/library/entries/"+id, "")
	require.Equal(t, http.StatusOK, w.Code)
	var got library.Entry
	e.data(t, w, &got)
	assert.Equal(t, "ls", got.Implementations["linux"].Code)

	w = e.do(t, http.MethodGet, "/library/entries", "")
	require.Equal(t, http.StatusOK, w.Code)
	var list struct {
		Data []library.Entry `json:"data"`
		Meta struct {
			Count int `json:"count"`
		} `json:"meta"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	assert.Equal(t, 1, list.Meta.Count)
	require.Len(t, list.Data, 1)
	assert.Empty(t, list.Data[0].Implementations)

	put := `{"name":"ls","message":"second","base_version":1,"implementations":{"linux":{"kind":"command","code":"ls -l"}}}`
	w = e.do(t, http.MethodPut, "/library/entries/"+id, put)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var saved library.Entry
	e.data(t, w, &saved)
	assert.Equal(t, 2, saved.ActiveVersion)

	// stale base version
	put = `{"name":"ls","message":"third","base_version":1,"implementations":{"linux":{"kind":"command","code":"ls -la"}}}`
	w = e.do(t, http.MethodPut, "/library/entries/"+id, put)
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())

	// missing base_version
	put = `{"name":"ls","implementations":{"linux":{"kind":"command","code":"ls -la"}}}`
	w = e.do(t, http.MethodPut, "/library/entries/"+id, put)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "base_version is required")

	w = e.do(t, http.MethodGet, "/library/entries/"+id+"/versions", "")
	require.Equal(t, http.StatusOK, w.Code)
	var versions []library.Version
	e.data(t, w, &versions)
	require.Len(t, versions, 2)
	assert.Equal(t, 2, versions[0].Version)
	assert.Equal(t, 1, versions[1].Version)

	w = e.do(t, http.MethodGet, "/library/entries/"+id+"/versions/1", "")
	require.Equal(t, http.StatusOK, w.Code)
	var v1 library.Version
	e.data(t, w, &v1)
	require.NotNil(t, v1.Content)
	assert.Equal(t, "ls", v1.Content.Implementations["linux"].Code)

	w = e.do(t, http.MethodPost, "/library/entries/"+id+"/versions/1/restore", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	w = e.do(t, http.MethodPost, "/library/entries/"+id+"/versions/1/restore", `{"base_version":2}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var restored library.Entry
	e.data(t, w, &restored)
	assert.Equal(t, 3, restored.ActiveVersion)
	assert.Equal(t, "ls", restored.Implementations["linux"].Code)

	w = e.do(t, http.MethodDelete, "/library/entries/"+id, "")
	require.Equal(t, http.StatusNoContent, w.Code)

	w = e.do(t, http.MethodGet, "/library/entries", "")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	assert.Empty(t, list.Data)

	w = e.do(t, http.MethodGet, "/library/entries?filter[deleted]=true", "")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	require.Len(t, list.Data, 1)

	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, "/library/entries/"+id, "").Code)
	assert.Equal(t, http.StatusOK, e.do(t, http.MethodGet, "/library/entries/"+id+"?include_deleted=true", "").Code)

	w = e.do(t, http.MethodPost, "/library/entries/"+id+"/undelete", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = e.do(t, http.MethodGet, "/library/entries", "")
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
	assert.Len(t, list.Data, 1)
}

func TestLibraryEntriesListFiltersAndErrors(t *testing.T) {
	e := newLibraryEntriesEnv(t)
	require.Equal(t, http.StatusCreated, e.do(t, http.MethodPost, "/library/entries", entryBody).Code)
	win := `{"name":"dir","tags":["x"],"implementations":{"windows":{"kind":"command","code":"dir"}}}`
	require.Equal(t, http.StatusCreated, e.do(t, http.MethodPost, "/library/entries", win).Code)

	count := func(q string) int {
		w := e.do(t, http.MethodGet, "/library/entries?"+q, "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var l struct {
			Data []library.Entry `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &l))
		return len(l.Data)
	}
	assert.Equal(t, 2, count(""))
	assert.Equal(t, 1, count("filter[os]=windows"))
	assert.Equal(t, 1, count("filter[tags]=x"))
	assert.Equal(t, 1, count("filter[name]=di"))
	assert.Equal(t, 0, count("filter[builtin]=true"))
	assert.Equal(t, 1, count("page[limit]=1"))

	assert.Equal(t, http.StatusNotFound, e.do(t, http.MethodGet, "/library/entries/nope", "").Code)
	assert.Equal(t, http.StatusBadRequest, e.do(t, http.MethodGet, "/library/entries/nope/versions/abc", "").Code)
	assert.Equal(t, http.StatusBadRequest, e.do(t, http.MethodGet, "/library/entries?filter[deleted]=maybe", "").Code)
	assert.Equal(t, http.StatusBadRequest, e.do(t, http.MethodPost, "/library/entries", `{"name":"x"}`).Code)
}

func TestLibraryEntriesPageLimitAndOSFilter(t *testing.T) {
	e := newLibraryEntriesEnv(t)
	for i := 0; i < 501; i++ {
		body := fmt.Sprintf(`{"name":"e%03d","implementations":{"linux":{"kind":"command","code":"ls"}}}`, i)
		require.Equal(t, http.StatusCreated, e.do(t, http.MethodPost, "/library/entries", body).Code)
	}
	list := func(q string) (int, int) {
		w := e.do(t, http.MethodGet, "/library/entries?"+q, "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var l struct {
			Data []library.Entry `json:"data"`
			Meta struct {
				Count int `json:"count"`
			} `json:"meta"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &l))
		return len(l.Data), l.Meta.Count
	}
	n, total := list("")
	assert.Equal(t, 100, n)
	assert.Equal(t, 501, total)
	n, _ = list("page[limit]=500")
	assert.Equal(t, 500, n)
	n, _ = list("page[limit]=501")
	assert.Equal(t, 500, n)

	w := e.do(t, http.MethodGet, "/library/entries?filter[os]=beos", "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "beos")
}

func TestLibraryEntryWriteBodyLimit(t *testing.T) {
	e := newLibraryEntriesEnv(t)
	e.al.config = &chconfig.Config{API: chconfig.APIConfig{MaxRequestBytes: 10 * 1024, MaxRequestBytesLibrary: 256 * 1024}}
	e.al.applyMaxBytes(e.router)

	big := func(name string, size int) string {
		return `{"name":"` + name + `","message":"m","implementations":{"linux":{"kind":"script","code":"` + strings.Repeat("a", size) + `"}}}`
	}
	form := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(api.WithUser(tenant.WithID(req.Context(), tenant.Default), "admin"))
		w := httptest.NewRecorder()
		e.router.ServeHTTP(w, req)
		return w
	}

	// 50 KB is over the generic limit but under the library limit
	w := e.do(t, http.MethodPost, "/library/entries", big("fifty", 50*1024))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var created library.Entry
	e.data(t, w, &created)

	w = e.do(t, http.MethodPut, "/library/entries/"+created.ID, strings.Replace(big("fifty", 60*1024), `"message"`, `"base_version":1,"message"`, 1))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// over the library limit gives the same error as the generic limit
	w = form(http.MethodPost, "/library/entries", big("huge", 300*1024))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Request data exceeds the limit of 262144 bytes")

	w = e.do(t, http.MethodPost, "/library/entries", big("huge", 300*1024))
	assert.GreaterOrEqual(t, w.Code, 400)
	assert.Contains(t, w.Body.String(), "request body too large")
}
