package chserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	jobsmigration "github.com/openrport/openrport/db/migration/jobs"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/routes"
	"github.com/openrport/openrport/server/test/jb"
	"github.com/openrport/openrport/share/tenant"
)

// multiJobAccessFixture stores a multi-client job created by "alice" with one job result per client.
func multiJobAccessFixture(t *testing.T, viewer string, viewerGroup string) (*APIListener, context.Context) {
	t.Helper()
	db, err := sqlite.New(":memory:", jobsmigration.FS, sqlite.DataSourceOptions{})
	require.NoError(t, err)
	jp := jobs.NewSqliteProvider(db, testLog)
	t.Cleanup(func() { jp.Close() })

	ctx := tenant.WithID(context.Background(), tenant.Default)
	mj := jb.NewMulti(t).JID("mj-alice").Build()
	mj.CreatedBy = "alice"
	require.NoError(t, jp.SaveMultiJob(ctx, mj))
	for _, cid := range []string{"c1", "c2"} {
		j := jb.New(t).JID("j-" + cid).ClientID(cid).MultiJobID("mj-alice").Build()
		j.CreatedBy = "alice"
		require.NoError(t, jp.SaveJob(ctx, j))
	}

	al := &APIListener{Logger: testLog, apiServices: &apiServices{}}
	al.jobProvider = jp
	al.userService = users.NewAPIService(users.NewStaticProvider([]*users.User{
		{Username: viewer, Groups: []string{viewerGroup}},
	}), false, 0, -1)
	return al, api.WithUser(ctx, viewer)
}

func TestMultiClientCommandJobsOnlyForCreatorOrAdmin(t *testing.T) {
	for _, tc := range []struct {
		name, viewer, group string
		wantStatus          int
	}{
		{"another non-admin user", "bob", "Operators", http.StatusForbidden},
		{"the creator", "alice", "Operators", http.StatusOK},
		{"an admin", "root", users.Administrators, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			al, ctx := multiJobAccessFixture(t, tc.viewer, tc.group)
			req := httptest.NewRequest(http.MethodGet, "/api/v1/commands/mj-alice/jobs", nil).WithContext(ctx)
			req = mux.SetURLVars(req, map[string]string{routes.ParamJobID: "mj-alice"})
			w := httptest.NewRecorder()

			al.handleGetMultiClientCommandJobs(w, req)

			assert.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			if tc.wantStatus == http.StatusForbidden {
				assert.NotContains(t, w.Body.String(), "j-c1")
			}
		})
	}
}

func TestMultiClientCommandsListShowsNonAdminsOnlyTheirOwn(t *testing.T) {
	for _, tc := range []struct {
		name, viewer, group string
		wantCount           int
	}{
		{"another non-admin user", "bob", "Operators", 0},
		{"the creator", "alice", "Operators", 1},
		{"an admin", "root", users.Administrators, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			al, ctx := multiJobAccessFixture(t, tc.viewer, tc.group)
			req := httptest.NewRequest(http.MethodGet, "/api/v1/commands", nil).WithContext(ctx)
			w := httptest.NewRecorder()

			al.handleGetMultiClientCommands(w, req)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var resp struct {
				Data []json.RawMessage `json:"data"`
				Meta struct {
					Count int `json:"count"`
				} `json:"meta"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			assert.Len(t, resp.Data, tc.wantCount)
			assert.Equal(t, tc.wantCount, resp.Meta.Count)
		})
	}
}
