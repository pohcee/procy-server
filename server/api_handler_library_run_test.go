package chserver

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	librarymigration "github.com/openrport/openrport/db/migration/library"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/tenant"
	"github.com/openrport/openrport/share/test"
	"github.com/openrport/openrport/share/ws"
)

type libraryRunEnv struct {
	*libraryEntriesEnv
	jp    *jobs.SqliteProvider
	store *library.Store
	conns []*test.ConnMock
}

func newLibraryRunEnv(t *testing.T) *libraryRunEnv {
	t.Helper()
	sequentialJobIDs(t)
	conns := []*test.ConnMock{
		makeConnMock(t, 1, time.Date(2020, 10, 10, 10, 10, 1, 0, time.UTC)),
		makeConnMock(t, 2, time.Date(2020, 10, 10, 10, 10, 2, 0, time.UTC)),
		makeConnMock(t, 3, time.Date(2020, 10, 10, 10, 10, 3, 0, time.UTC)),
	}
	cls := []*clientdata.Client{
		clients.New(t).ID("c-linux").Connection(conns[0]).Logger(testLog).Build(),
		clients.New(t).ID("c-win").Connection(conns[1]).Logger(testLog).Build(),
		clients.New(t).ID("c-mac").Connection(conns[2]).Logger(testLog).Build(),
	}
	cls[1].OSKernel = "windows"
	cls[2].OSKernel = "darwin"

	al := makeAPIListener(makeTestUser("admin"), clients.NewClientRepository(cls, &hour, testLog), 60, nil, testLog)
	jp := makeJobsProvider(t, DataSourceOptions, testLog)
	t.Cleanup(func() { jp.Close() })
	al.jobProvider = jp
	al.clientGroupProvider = makeGroupsProvider(t, DataSourceOptions)

	db, err := sqlite.New(filepath.Join(t.TempDir(), "library.db"), librarymigration.AssetNames(), librarymigration.Asset, sqlite.DataSourceOptions{WALEnabled: false})
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := library.NewStore(db)
	al.libraryManager = library.NewManager(store, func() time.Time { now = now.Add(time.Minute); return now })

	r := mux.NewRouter()
	al.addLibraryEntriesRoutes(r)
	return &libraryRunEnv{libraryEntriesEnv: &libraryEntriesEnv{router: r, al: al}, jp: jp, store: store, conns: conns}
}

const runEntryBody = `{"name":"up","message":"m","implementations":{` +
	`"linux":{"kind":"command","code":"uptime","timeout_sec":45},` +
	`"windows":{"kind":"command","code":"hostname","interpreter":"powershell","timeout_sec":30}}}`

func (e *libraryRunEnv) createEntry(t *testing.T, body string) library.Entry {
	t.Helper()
	w := e.do(t, http.MethodPost, "/library/entries", body)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var entry library.Entry
	e.data(t, w, &entry)
	return entry
}

func (e *libraryRunEnv) waitJobs(t *testing.T, jid string, n int) *models.MultiJob {
	t.Helper()
	var mj *models.MultiJob
	require.Eventually(t, func() bool {
		var err error
		mj, err = e.jp.GetMultiJob(tenant.WithID(context.Background(), tenant.Default), jid)
		return err == nil && mj != nil && len(mj.Jobs) == n
	}, 5*time.Second, 10*time.Millisecond)
	return mj
}

func TestRunEntryStartsMultiJob(t *testing.T) {
	e := newLibraryRunEnv(t)
	entry := e.createEntry(t, runEntryBody)

	w := e.do(t, http.MethodPost, "/library/entries/"+entry.ID+"/run",
		`{"client_ids":["c-linux","c-win","c-mac"],"execute_concurrently":true}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		JID string `json:"jid"`
	}
	e.data(t, w, &resp)
	require.NotEmpty(t, resp.JID)

	mj := e.waitJobs(t, resp.JID, 3)
	assert.Equal(t, entry.ID, mj.LibraryEntryID)
	assert.Equal(t, 1, mj.LibraryVersion)
	assert.Equal(t, "admin", mj.CreatedBy)

	by := map[string]*models.Job{}
	for _, j := range mj.Jobs {
		by[j.ClientID] = j
		assert.Equal(t, entry.ID, j.LibraryEntryID)
		assert.Equal(t, 1, j.LibraryVersion)
	}
	assert.Equal(t, "linux", by["c-linux"].OS)
	assert.Equal(t, "uptime", by["c-linux"].Command)
	assert.Equal(t, 45, by["c-linux"].TimeoutSec)
	assert.Equal(t, "windows", by["c-win"].OS)
	assert.Equal(t, "hostname", by["c-win"].Command)
	assert.Equal(t, models.JobStatusRunning, by["c-linux"].Status)

	mac := by["c-mac"]
	assert.Equal(t, models.JobStatusSkipped, mac.Status)
	assert.Equal(t, "darwin", mac.OS)
	require.NotNil(t, mac.Result)
	assert.Equal(t, "no macOS implementation", mac.Result.Summary)
}

func TestRunUsesVersionAtStart(t *testing.T) {
	e := newLibraryRunEnv(t)
	entry := e.createEntry(t, runEntryBody)

	w := e.do(t, http.MethodPost, "/library/entries/"+entry.ID+"/run", `{"client_ids":["c-linux"]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		JID string `json:"jid"`
	}
	e.data(t, w, &resp)

	// a later save must not change what the started run recorded
	v2 := strings.Replace(runEntryBody, `"name":"up","message":"m"`, `"name":"up","message":"m2","base_version":1`, 1)
	v2 = strings.Replace(v2, "uptime", "uptime -p", 1)
	w = e.do(t, http.MethodPut, "/library/entries/"+entry.ID, v2)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	mj := e.waitJobs(t, resp.JID, 1)
	assert.Equal(t, 1, mj.LibraryVersion)
	assert.Equal(t, 1, mj.Jobs[0].LibraryVersion)
	assert.Equal(t, "uptime", mj.Jobs[0].Command)

	w = e.do(t, http.MethodPost, "/library/entries/"+entry.ID+"/run", `{"client_ids":["c-linux"]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	e.data(t, w, &resp)
	mj = e.waitJobs(t, resp.JID, 1)
	assert.Equal(t, 2, mj.LibraryVersion)
}

func TestRunDeletedEntry404(t *testing.T) {
	e := newLibraryRunEnv(t)
	entry := e.createEntry(t, runEntryBody)
	require.Equal(t, http.StatusNoContent, e.do(t, http.MethodDelete, "/library/entries/"+entry.ID, "").Code)

	w := e.do(t, http.MethodPost, "/library/entries/"+entry.ID+"/run", `{"client_ids":["c-linux"]}`)
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	w = e.do(t, http.MethodPost, "/library/entries/nope/run", `{"client_ids":["c-linux"]}`)
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestRunEntryNoTargets400(t *testing.T) {
	e := newLibraryRunEnv(t)
	entry := e.createEntry(t, runEntryBody)
	w := e.do(t, http.MethodPost, "/library/entries/"+entry.ID+"/run", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestRunEntryNoImplementationForAnyTarget400(t *testing.T) {
	e := newLibraryRunEnv(t)
	entry := e.createEntry(t, runEntryBody)

	// c-mac is the only target and the entry has no macOS implementation
	w := e.do(t, http.MethodPost, "/library/entries/"+entry.ID+"/run", `{"client_ids":["c-mac"]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `entry \"up\" has no implementation for any target (targets darwin: 1)`)

	// a mixed run still starts, the unsupported device is skipped
	w = e.do(t, http.MethodPost, "/library/entries/"+entry.ID+"/run", `{"client_ids":["c-mac","c-linux"]}`)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

func TestResultKeepsLibraryFields(t *testing.T) {
	jp := makeJobsProvider(t, DataSourceOptions, testLog)
	t.Cleanup(func() { jp.Close() })
	stored := &models.Job{
		JID: "j1", ClientID: "c1", Status: models.JobStatusRunning, StartedAt: time.Now(),
		LibraryEntryID: "e1", LibraryVersion: 4, OS: "linux",
	}
	require.NoError(t, jp.CreateJob(tenant.WithID(context.Background(), tenant.Default), stored))

	cl := &ClientListener{logger: testLog, server: &Server{jobProvider: jp, uiJobWebSockets: ws.NewWebSocketCache()}}
	// an older client does not echo the library fields
	got, err := cl.saveCmdResult(tenant.WithID(context.Background(), tenant.Default), []byte(`{"jid":"j1","client_id":"c1","status":"successful","started_at":"2026-01-01T00:00:00Z"}`))
	require.NoError(t, err)
	assert.Equal(t, "e1", got.LibraryEntryID)
	assert.Equal(t, 4, got.LibraryVersion)
	assert.Equal(t, "linux", got.OS)

	saved, err := jp.GetByJID(tenant.WithID(context.Background(), tenant.Default), "c1", "j1")
	require.NoError(t, err)
	assert.Equal(t, models.JobStatusSuccessful, saved.Status)
	assert.Equal(t, "e1", saved.LibraryEntryID)
	assert.Equal(t, 4, saved.LibraryVersion)
	assert.Equal(t, "linux", saved.OS)

	// values the client does send win
	got, err = cl.saveCmdResult(tenant.WithID(context.Background(), tenant.Default), []byte(`{"jid":"j1","client_id":"c1","status":"successful","library_entry_id":"e2","library_version":5,"os":"windows"}`))
	require.NoError(t, err)
	assert.Equal(t, "e2", got.LibraryEntryID)
	assert.Equal(t, 5, got.LibraryVersion)
	assert.Equal(t, "windows", got.OS)

	// unknown job: nothing to keep, still saved
	_, err = cl.saveCmdResult(tenant.WithID(context.Background(), tenant.Default), []byte(`{"jid":"j9","client_id":"c1","status":"successful"}`))
	require.NoError(t, err)
}

func (e *libraryRunEnv) syncBuiltins(t *testing.T) *library.Entry {
	t.Helper()
	builtins, err := library.Builtins()
	require.NoError(t, err)
	_, _, _, err = library.SyncBuiltins(tenant.WithID(context.Background(), tenant.Default), e.store, builtins, "test", time.Now())
	require.NoError(t, err)
	entry, err := e.store.ByBuiltinKey(tenant.WithID(context.Background(), tenant.Default), "client-update")
	require.NoError(t, err)
	require.NotNil(t, entry)
	return entry
}

func TestBuiltinWithoutServerURLFails(t *testing.T) {
	e := newLibraryRunEnv(t)
	entry := e.syncBuiltins(t)

	w := e.do(t, http.MethodPost, "/library/entries/"+entry.ID+"/run", `{"client_ids":["c-linux"]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `enable [pairing] and set its url (or api.base_url) to use the built-in \"Client update\" entry`)

	// a URL alone is not enough while pairing is disabled
	e.al.config.Pairing.URL = "https://rport.example.com/"
	w = e.do(t, http.MethodPost, "/library/entries/"+entry.ID+"/run", `{"client_ids":["c-linux"]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `enable [pairing]`)

	// a target without an implementation does not need the URL
	w = e.do(t, http.MethodPost, "/library/entries/"+entry.ID+"/run", `{"client_ids":["c-mac"]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "no implementation for any target")
}

func TestBuiltinWithServerURLRuns(t *testing.T) {
	e := newLibraryRunEnv(t)
	e.al.config.Pairing.Enabled = true
	e.al.config.Pairing.URL = "https://rport.example.com/"
	entry := e.syncBuiltins(t)

	w := e.do(t, http.MethodPost, "/library/entries/"+entry.ID+"/run", `{"client_ids":["c-linux"]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		JID string `json:"jid"`
	}
	e.data(t, w, &resp)
	got := e.waitJobs(t, resp.JID, 1)
	assert.Contains(t, got.Jobs[0].Command, "https://rport.example.com/pairing/update")
	assert.NotContains(t, got.Jobs[0].Command, "{{server_url}}")
}

func TestBuiltinServedWithServerURL(t *testing.T) {
	e := newLibraryRunEnv(t)
	entry := e.syncBuiltins(t)
	const want = "https://rport.example.com/pairing/update"

	// without a usable URL the placeholder stays
	w := e.do(t, http.MethodGet, "/library/entries/"+entry.ID, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "{{server_url}}")

	e.al.config.Pairing.Enabled = true
	e.al.config.Pairing.URL = "https://rport.example.com/"

	user := e.createEntry(t, `{"name":"raw","message":"m","implementations":{"linux":{"kind":"command","code":"echo {{server_url}}"}}}`)

	for _, path := range []string{
		"/library/entries/" + entry.ID,
		"/library/entries/" + entry.ID + "/versions/" + strconv.Itoa(entry.ActiveVersion),
	} {
		w = e.do(t, http.MethodGet, path, "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Contains(t, w.Body.String(), want, path)
		assert.NotContains(t, w.Body.String(), "{{server_url}}", path)
	}

	// storage keeps the placeholder
	stored, err := e.store.ByBuiltinKey(tenant.WithID(context.Background(), tenant.Default), "client-update")
	require.NoError(t, err)
	assert.Contains(t, stored.Implementations["linux"].Code, "{{server_url}}")
	v, err := e.store.Version(tenant.WithID(context.Background(), tenant.Default), entry.ID, entry.ActiveVersion)
	require.NoError(t, err)
	assert.Contains(t, v.Content.Implementations["linux"].Code, "{{server_url}}")

	// the compat view path serves the concrete URL too
	r := defaultTenantRequest(http.MethodGet, "/")
	got, found, err := e.al.libraryViewEntry(r, entry.ID, library.KindScript)
	require.NoError(t, err)
	if !found {
		got, found, err = e.al.libraryViewEntry(r, entry.ID, library.KindCommand)
		require.NoError(t, err)
	}
	require.True(t, found)
	assert.Contains(t, got.Implementations["linux"].Code, want)

	// user entries are returned verbatim
	w = e.do(t, http.MethodGet, "/library/entries/"+user.ID, "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "echo {{server_url}}")
}
