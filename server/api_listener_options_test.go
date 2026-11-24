package chserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api/jobs/schedule"
	"github.com/openrport/openrport/server/auditlog"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clients/targeting"
	"github.com/openrport/openrport/server/jobrunner"
	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/share/files"
)

// The API listener is built from its options alone: no Server and no client listener. Work on
// clients would go through the given dispatcher.
func TestNewAPIListenerFromOptions(t *testing.T) {
	ctx := storageTestCtx()
	config := storageTestConfig(t)
	repos, storage, err := openSQLiteRepositories(ctx, config, nil, false, testLog)
	require.NoError(t, err)
	t.Cleanup(func() { _ = storage.closeAll() })

	clientService := clients.NewClientService(nil, nil, clients.NewClientRepository(nil, nil, testLog), testLog, nil)
	d := &recordingDispatcher{}
	lib := library.NewManager(repos.Library, time.Now)
	runner, err := jobrunner.New(jobrunner.Options{
		Jobs:       repos.Jobs,
		Dispatcher: d,
		Targets:    targeting.Resolver{Clients: clientService, Groups: repos.ClientGroups},
		Library:    lib,
		Config:     config,
		Done:       jobrunner.NewDoneChannels(),
		Logger:     testLog,
	})
	require.NoError(t, err)
	schedules := schedule.NewManager(ctx, runner, repos.Schedules, testLog, 30)
	audit, err := auditlog.New(ctx, testLog, clientService, config.API.AuditLog, repos.AuditLog)
	require.NoError(t, err)

	al, err := NewAPIListener(ctx, APIListenerOptions{
		Config:        config,
		Fingerprint:   "fp",
		Repositories:  repos,
		ClientService: clientService,
		Dispatcher:    d,
		JobRunner:     runner,
		Library:       lib,
		Schedules:     schedules,
		AuditLog:      audit,
		Files:         files.NewFileSystem(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = al.Close() })
	assert.Same(t, d, al.dispatcher)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.SetBasicAuth("admin", "foobaz")
	w := httptest.NewRecorder()
	al.router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"fingerprint":"fp"`)
}

func TestNewAPIListenerRequiresOptions(t *testing.T) {
	_, err := NewAPIListener(storageTestCtx(), APIListenerOptions{})
	assert.ErrorContains(t, err, "Config is required")
}
