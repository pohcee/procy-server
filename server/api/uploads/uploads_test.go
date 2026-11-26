package uploads

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/server/api/users"
	"github.com/pohcee/procy-server/server/auditlog"
	auditlogconfig "github.com/pohcee/procy-server/server/auditlog/config"
	"github.com/pohcee/procy-server/server/clients/clientdata"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/models"
	"github.com/pohcee/procy-server/share/tenant"
)

var testLog = logger.NewLogger("uploads-test", logger.LogOutput{File: os.Stdout}, logger.LogLevelDebug)

type noClients struct{}

func (noClients) GetByID(context.Context, string) (*clientdata.Client, error) { return nil, nil }

func TestUploadAuditEntryIsSavedForTheClientsTenant(t *testing.T) {
	ctx := tenant.WithID(context.Background(), tenant.Default)
	cfg := auditlogconfig.Config{Enable: true, Rotation: auditlogconfig.RotationMonthly}
	p, err := auditlog.NewRotationProvider(ctx, testLog, cfg.RotationPeriod(), t.TempDir(), sqlite.DataSourceOptions{})
	require.NoError(t, err)
	al, err := auditlog.New(ctx, testLog, noClients{}, cfg, p)
	require.NoError(t, err)
	t.Cleanup(func() { al.Close() })
	h := New(Options{AuditLog: al, Logger: testLog})

	client := &clientdata.Client{}
	client.SetID("c1")
	client.Context = tenant.WithID(context.Background(), "a")
	results := make(chan *uploadResult, 1)
	results <- &uploadResult{client: client, resp: &models.UploadResponse{}}
	close(results)
	h.consumeUploadResults(tenant.WithID(context.Background(), "a"), results, &UploadRequest{UploadedFile: &models.UploadedFile{ID: "u1"}})

	admin := &users.User{Username: "admin", Groups: []string{users.Administrators}}
	r := httptest.NewRequest("GET", "/auditlog", nil).WithContext(tenant.WithID(context.Background(), "a"))
	res, err := al.List(r, admin)
	require.NoError(t, err)
	entries := res.Data.([]*auditlog.Entry)
	require.Len(t, entries, 1)
	assert.Equal(t, "u1", entries[0].ID)
}

// A client connected to another node (cluster) has no connection context here: the entry keeps the tenant of the
// upload request.
func TestUploadAuditEntryOfARemoteClientKeepsTheRequestsTenant(t *testing.T) {
	ctx := tenant.WithID(context.Background(), tenant.Default)
	cfg := auditlogconfig.Config{Enable: true, Rotation: auditlogconfig.RotationMonthly}
	p, err := auditlog.NewRotationProvider(ctx, testLog, cfg.RotationPeriod(), t.TempDir(), sqlite.DataSourceOptions{})
	require.NoError(t, err)
	al, err := auditlog.New(ctx, testLog, noClients{}, cfg, p)
	require.NoError(t, err)
	t.Cleanup(func() { al.Close() })
	h := New(Options{AuditLog: al, Logger: testLog})

	remote := &clientdata.Client{}
	remote.SetID("c1")
	results := make(chan *uploadResult, 2)
	results <- &uploadResult{client: remote, resp: &models.UploadResponse{}}
	results <- &uploadResult{client: remote, err: errors.New("refused")}
	close(results)
	h.consumeUploadResults(tenant.WithID(context.Background(), "a"), results, &UploadRequest{UploadedFile: &models.UploadedFile{ID: "u1"}})

	admin := &users.User{Username: "admin", Groups: []string{users.Administrators}}
	r := httptest.NewRequest("GET", "/auditlog", nil).WithContext(tenant.WithID(context.Background(), "a"))
	res, err := al.List(r, admin)
	require.NoError(t, err)
	assert.Len(t, res.Data.([]*auditlog.Entry), 2)
}
