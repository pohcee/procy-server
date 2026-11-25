package chserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/api/jobs/schedule"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/server/routes"
	"github.com/openrport/openrport/server/test/jb"
	"github.com/openrport/openrport/share/security"
	"github.com/openrport/openrport/share/tenant"
)

// Users of the runs fixture (password = username):
//   - alice and bob are in Schedulers (commands + scheduler); alice created every job and the schedule
//   - carol is in Operators (commands only, no scheduler)
//   - dave is in Outsiders (commands only, no device access)
//   - erin is in Planners (commands + scheduler, but no access to c1 or c2)
//   - root is an administrator
//
// Devices: c1 and c2 are open to Schedulers and Operators; c3 to Outsiders only.
const runsFixtureSchedule = "sched-1"

// runsFixture serves the real API router with a user store that evaluates group permissions (when groupPermissions)
// or one that cannot (a static store), over SQLite jobs, schedules and client groups.
func runsFixture(t *testing.T, groupPermissions bool) *APIListener {
	t.Helper()
	ctx := tenant.WithID(context.Background(), tenant.Default)

	jp := makeJobsProvider(t, sqlite.DataSourceOptions{}, testLog)
	t.Cleanup(func() { jp.Close() })
	sp := schedule.NewSQLiteProvider(jobsDBOf(t, jp))
	require.NoError(t, sp.Insert(ctx, &schedule.Schedule{Base: schedule.Base{
		ID: runsFixtureSchedule, CreatedAt: time.Now(), CreatedBy: "alice", Name: "nightly", Schedule: "0 1 * * *", Type: schedule.TypeCommand,
	}}))

	saveMulti := func(jid, createdBy, scheduleID string, startedAt time.Time) {
		b := jb.NewMulti(t).JID(jid).ClientIDs("c1", "c2").StartedAt(startedAt)
		if scheduleID != "" {
			b = b.ScheduleID(scheduleID)
		}
		mj := b.Build()
		mj.CreatedBy = createdBy
		require.NoError(t, jp.SaveMultiJob(ctx, mj))
		for _, cid := range []string{"c1", "c2"} {
			j := jb.New(t).JID("j-" + jid + "-" + cid).ClientID(cid).MultiJobID(jid).Build()
			j.CreatedBy = createdBy
			require.NoError(t, jp.SaveJob(ctx, j))
		}
	}
	now := time.Now().UTC()
	saveMulti("mj-sched", "alice", runsFixtureSchedule, now.Add(-4*time.Minute)) // a run of an existing schedule
	saveMulti("mj-adhoc", "alice", "", now.Add(-3*time.Minute))                  // alice's own ad-hoc multi-job
	saveMulti("mj-gone", "alice", "sched-deleted", now.Add(-2*time.Minute))      // run of a schedule that no longer exists
	saveMulti("mj-bob", "bob", "", now.Add(-1*time.Minute))                      // bob's own ad-hoc multi-job
	single := jb.New(t).JID("j-single").ClientID("c1").Build()                   // a single-device job by alice
	single.CreatedBy = "alice"
	require.NoError(t, jp.SaveJob(ctx, single))
	single3 := jb.New(t).JID("j-single-c3").ClientID("c3").Build()
	single3.CreatedBy = "alice"
	require.NoError(t, jp.SaveJob(ctx, single3))

	var userService UserService
	if groupPermissions {
		userService = users.NewAPIService(runsUserDatabase(t), false, 0, -1)
	} else {
		userService = users.NewAPIService(users.NewStaticProvider([]*users.User{
			{Username: "alice", Password: "alice", Groups: []string{"Schedulers"}},
			{Username: "bob", Password: "bob", Groups: []string{"Schedulers"}},
			{Username: "root", Password: "root", Groups: []string{users.Administrators}},
		}), false, 0, -1)
	}

	keep := time.Hour
	devices := []*clientdata.Client{
		clients.New(t).ID("c1").AllowedUserGroups([]string{"Schedulers", "Operators"}).Logger(testLog).Build(),
		clients.New(t).ID("c2").AllowedUserGroups([]string{"Schedulers", "Operators"}).Logger(testLog).Build(),
		clients.New(t).ID("c3").AllowedUserGroups([]string{"Outsiders"}).Logger(testLog).Build(),
	}
	al := &APIListener{
		Logger: testLog,
		apiServices: &apiServices{
			config:              &chconfig.Config{API: chconfig.APIConfig{MaxRequestBytes: 1024 * 1024}},
			clientService:       clients.NewClientService(nil, nil, clients.NewClientRepository(devices, &keep, testLog), testLog, nil),
			clientGroupProvider: makeGroupsProvider(t, sqlite.DataSourceOptions{}),
			jobProvider:         jp,
			scheduleManager:     schedule.NewManager(ctx, nil, sp, testLog, 30),
		},
		bannedUsers: security.NewBanList(0),
		userService: &MockUsersService{UserService: userService},
	}
	useLocalDispatcher(al)
	al.initRouter()
	return al
}

func runsUserDatabase(t *testing.T) *users.UserDatabase {
	t.Helper()
	db, err := sqlx.Connect("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`CREATE TABLE "users" ("username" TEXT PRIMARY KEY, "password" TEXT, "password_expired" BOOLEAN NOT NULL CHECK (password_expired IN (0, 1)) DEFAULT 0)`,
		`CREATE TABLE "groups" ("username" TEXT, "group" TEXT)`,
		`CREATE TABLE "group_details" ("name" TEXT, "permissions" TEXT)`,
		`CREATE UNIQUE INDEX "main"."username_group_name" ON "group_details" ("name" ASC)`,
		`INSERT INTO "users" VALUES ('alice','alice',false), ('bob','bob',false), ('carol','carol',false), ('dave','dave',false), ('erin','erin',false), ('root','root',false)`,
		`INSERT INTO "groups" VALUES ('alice','Schedulers'), ('bob','Schedulers'), ('carol','Operators'), ('dave','Outsiders'), ('erin','Planners'), ('root','` + users.Administrators + `')`,
		`INSERT INTO "group_details" VALUES ('Schedulers','{"commands":true,"scheduler":true}'), ('Operators','{"commands":true}'),
			('Outsiders','{"commands":true}'), ('Planners','{"commands":true,"scheduler":true}'), ('` + users.Administrators + `','{"commands":true,"scheduler":true}')`,
	} {
		_, err := db.Exec(q)
		require.NoError(t, err, q)
	}
	p, err := users.NewUserDatabase(db, "users", "groups", "group_details", false, false, false, testLog)
	require.NoError(t, err)
	return p
}

func runsGet(t *testing.T, al *APIListener, user, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.SetBasicAuth(user, user)
	w := httptest.NewRecorder()
	al.router.ServeHTTP(w, req)
	return w
}

// runsListIDs decodes a list response into its items' jid and its meta count.
func runsListIDs(t *testing.T, w *httptest.ResponseRecorder) ([]string, int) {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		Data []struct {
			JID string `json:"jid"`
		} `json:"data"`
		Meta struct {
			Count int `json:"count"`
		} `json:"meta"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	ids := make([]string, 0, len(resp.Data))
	for _, d := range resp.Data {
		ids = append(ids, d.JID)
	}
	sort.Strings(ids)
	return ids, resp.Meta.Count
}

func TestScheduledRunVisibleToUsersWhoMaySeeTheSchedule(t *testing.T) {
	al := runsFixture(t, true)
	for _, tc := range []struct {
		name, viewer, jid string
		wantStatus        int
	}{
		{"scheduler user sees a run of another user's schedule", "bob", "mj-sched", http.StatusOK},
		{"admin sees the run", "root", "mj-sched", http.StatusOK},
		{"creator sees the run", "alice", "mj-sched", http.StatusOK},
		{"user without the scheduler permission does not", "carol", "mj-sched", http.StatusForbidden},
		{"scheduler user does not see another user's ad-hoc multi-job", "bob", "mj-adhoc", http.StatusForbidden},
		{"scheduler user does not see a run of a schedule that no longer exists", "bob", "mj-gone", http.StatusForbidden},
		{"user without the scheduler permission still does not see ad-hoc multi-jobs", "carol", "mj-adhoc", http.StatusForbidden},
		{"scheduler user without access to the run's devices does not see their results", "erin", "mj-sched", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := runsGet(t, al, tc.viewer, "/api/v1/commands/"+tc.jid)
			assert.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			if tc.wantStatus == http.StatusForbidden {
				assert.NotContains(t, w.Body.String(), "j-"+tc.jid)
			}

			w = runsGet(t, al, tc.viewer, "/api/v1/commands/"+tc.jid+"/jobs")
			assert.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			if tc.wantStatus == http.StatusOK {
				ids, count := runsListIDs(t, w)
				assert.Equal(t, []string{"j-" + tc.jid + "-c1", "j-" + tc.jid + "-c2"}, ids)
				assert.Equal(t, 2, count)
			} else {
				assert.NotContains(t, w.Body.String(), "j-"+tc.jid)
			}
		})
	}
}

func TestMultiJobListShowsSchedulerUsersTheRunsOfSchedules(t *testing.T) {
	al := runsFixture(t, true)
	for _, tc := range []struct {
		name, viewer string
		want         []string
	}{
		{"scheduler user: own multi-jobs and runs of existing schedules", "bob", []string{"mj-bob", "mj-sched"}},
		{"creator: own multi-jobs", "alice", []string{"mj-adhoc", "mj-gone", "mj-sched"}},
		{"user without the scheduler permission: own multi-jobs only", "carol", []string{}},
		{"admin: everything", "root", []string{"mj-adhoc", "mj-bob", "mj-gone", "mj-sched"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids, count := runsListIDs(t, runsGet(t, al, tc.viewer, "/api/v1/commands"))
			assert.Equal(t, tc.want, ids)
			assert.Equal(t, len(tc.want), count)
		})
	}

	t.Run("count stays the total under pagination and other filters", func(t *testing.T) {
		ids, count := runsListIDs(t, runsGet(t, al, "bob", "/api/v1/commands?page[limit]=1"))
		assert.Len(t, ids, 1)
		assert.Equal(t, 2, count)

		ids, count = runsListIDs(t, runsGet(t, al, "bob", "/api/v1/commands?filter[created_by]=alice"))
		assert.Equal(t, []string{"mj-sched"}, ids)
		assert.Equal(t, 1, count)
	})

	t.Run("the server-side filter cannot be sent by a client", func(t *testing.T) {
		w := runsGet(t, al, "carol", "/api/v1/commands?filter[created_by_or_scheduled]=carol")
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	})
}

func TestScheduledRunsNeedGroupPermissions(t *testing.T) {
	// A user store that cannot evaluate group permissions denies the scheduler permission to non-admins (as
	// permissionsMiddleware does), so bob keeps the creator-or-admin rule. The handlers are called directly: in front of
	// them permissionsMiddleware(commands) would already refuse bob.
	al := runsFixture(t, false)
	ctx := api.WithUser(tenant.WithID(context.Background(), tenant.Default), "bob")
	for name, handler := range map[string]http.HandlerFunc{
		"/api/v1/commands/mj-sched":      al.handleGetMultiClientCommand,
		"/api/v1/commands/mj-sched/jobs": al.handleGetMultiClientCommandJobs,
	} {
		req := httptest.NewRequest(http.MethodGet, name, nil).WithContext(ctx)
		req = mux.SetURLVars(req, map[string]string{routes.ParamJobID: "mj-sched"})
		w := httptest.NewRecorder()
		handler(w, req)
		assert.Equal(t, http.StatusForbidden, w.Code, name+": "+w.Body.String())
		assert.NotContains(t, w.Body.String(), "j-mj-sched", name)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/commands", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	al.handleGetMultiClientCommands(w, req)
	ids, count := runsListIDs(t, w)
	assert.Equal(t, []string{"mj-bob"}, ids)
	assert.Equal(t, 1, count)

	assert.Equal(t, http.StatusOK, runsGet(t, al, "root", "/api/v1/commands/mj-sched").Code)
}

func TestSingleDeviceJobsFollowDeviceAccess(t *testing.T) {
	al := runsFixture(t, true)
	for _, tc := range []struct {
		name, viewer, client, jid string
		wantStatus                int
	}{
		{"non-creator with device access sees the job", "carol", "c1", "j-single", http.StatusOK},
		{"non-creator with device access sees a multi-job's result on the device", "carol", "c1", "j-mj-adhoc-c1", http.StatusOK},
		{"creator sees the job", "alice", "c1", "j-single", http.StatusOK},
		{"admin sees the job", "root", "c1", "j-single", http.StatusOK},
		{"user without device access does not", "dave", "c1", "j-single", http.StatusForbidden},
		{"creator without device access does not", "alice", "c3", "j-single-c3", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := runsGet(t, al, tc.viewer, "/api/v1/clients/"+tc.client+"/commands/"+tc.jid)
			assert.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			if tc.wantStatus == http.StatusOK {
				assert.Contains(t, w.Body.String(), tc.jid)
			} else {
				assert.NotContains(t, w.Body.String(), tc.jid)
			}

			w = runsGet(t, al, tc.viewer, "/api/v1/clients/"+tc.client+"/commands")
			assert.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			if tc.wantStatus == http.StatusOK {
				ids, _ := runsListIDs(t, w)
				assert.Contains(t, ids, tc.jid)
			} else {
				assert.NotContains(t, w.Body.String(), tc.jid)
			}
		})
	}
}
