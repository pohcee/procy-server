package chserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/membership"
	"github.com/openrport/openrport/share/tenant"
)

// A server without a membership service (standalone) has no organization routes, reports no organizations
// capability and answers logins as before.
func TestNoOrganizationRoutesWithoutMemberships(t *testing.T) {
	al := &APIListener{
		insecureForTests: true,
		apiServices: &apiServices{
			config: &chconfig.Config{API: chconfig.APIConfig{MaxRequestBytes: 1024 * 1024}},
		},
		Logger: testLog,
	}
	al.initRouter()
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/me/organizations"},
		{http.MethodPost, "/api/v1/me/organization"},
		{http.MethodPost, "/api/v1/organization/members"},
	} {
		w := httptest.NewRecorder()
		al.router.ServeHTTP(w, httptest.NewRequest(rt.method, rt.path, nil))
		assert.Equal(t, http.StatusNotFound, w.Code, "%s %s", rt.method, rt.path)
	}

	data := getStatusData(t, newStatusTestListener(&chconfig.Config{}, nil))
	assert.NotContains(t, string(data["capabilities"]), "organizations")

	tok := "t"
	resp, err := al.withOrganizations(tenant.WithID(context.Background(), tenant.Default), "alice", loginResponse{Token: &tok})
	require.NoError(t, err)
	b, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.JSONEq(t, `{"token":"t","two_fa":null}`, string(b))
}

type fakeMemberships struct {
	membership.Service
	orgs       []membership.Organization
	elsewhere  bool
	remembered []tenant.ID
}

func (f *fakeMemberships) Organizations(context.Context, string) ([]membership.Organization, error) {
	return f.orgs, nil
}

func (f *fakeMemberships) MemberElsewhere(context.Context, string) (bool, error) {
	return f.elsewhere, nil
}

func (f *fakeMemberships) Remember(ctx context.Context, _ string) error {
	id, _ := tenant.FromContext(ctx)
	f.remembered = append(f.remembered, id)
	return nil
}

func TestLoginResponseListsOrganizationsOfSeveral(t *testing.T) {
	ctx := tenant.WithID(context.Background(), "b")
	tok := "t"
	f := &fakeMemberships{orgs: []membership.Organization{{ID: "a", Name: "A"}}}
	al := &APIListener{memberships: f}
	resp, err := al.withOrganizations(ctx, "alice", loginResponse{Token: &tok})
	require.NoError(t, err)
	assert.Nil(t, resp.Organizations, "one organization: as before")
	assert.Empty(t, f.remembered)

	f.orgs = append(f.orgs, membership.Organization{ID: "b", Name: "B"})
	resp, err = al.withOrganizations(ctx, "alice", loginResponse{Token: &tok})
	require.NoError(t, err)
	b, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.JSONEq(t, `{"token":"t","two_fa":null,"organization":"b","organizations":[{"id":"a","name":"A"},{"id":"b","name":"B"}]}`, string(b))
	assert.Equal(t, []tenant.ID{"b"}, f.remembered)
}

func TestAdminsDontChangeSharedAccounts(t *testing.T) {
	expired := true
	for _, tc := range []struct {
		name      string
		elsewhere bool
		change    *users.User
		refused   bool
	}{
		{"password of a shared account", true, &users.User{Password: "new password"}, true},
		{"expiry of a shared account", true, &users.User{PasswordExpired: &expired}, true},
		{"name of a shared account", true, &users.User{Username: "other"}, true},
		{"TOTP reset of a shared account", true, nil, true},
		{"groups of a shared account", true, &users.User{Groups: []string{"g"}}, false},
		{"same name, groups", true, &users.User{Username: "alice", Groups: []string{"g"}}, false},
		{"password of this organization's login", false, &users.User{Password: "new password"}, false},
	} {
		al := &APIListener{memberships: &fakeMemberships{elsewhere: tc.elsewhere}, apiServices: &apiServices{}, Logger: testLog}
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/v1/users/alice", nil).WithContext(tenant.WithID(context.Background(), "a"))
		assert.Equal(t, tc.refused, al.refuseSharedAccountChange(w, req, "alice", tc.change), tc.name)
		if tc.refused {
			assert.Equal(t, http.StatusForbidden, w.Code, tc.name)
		}
	}
	// standalone: never
	al := &APIListener{}
	assert.False(t, al.refuseSharedAccountChange(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/", nil), "alice", &users.User{Password: "x"}))
}

func TestLoginOfSeveralOrganizationsNamingNoneGetsTheListOnly(t *testing.T) {
	f := &fakeMemberships{orgs: []membership.Organization{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}}}
	al := &APIListener{memberships: f, apiServices: &apiServices{}, Logger: testLog}
	ctx := tenant.WithID(context.Background(), "a")

	w := httptest.NewRecorder()
	require.True(t, al.answerOrganizationChoice(w, httptest.NewRequest(http.MethodPost, "/api/v1/login", nil).WithContext(ctx), "alice"))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"data":{"token":null,"two_fa":null,"organizations":[{"id":"a","name":"A"},{"id":"b","name":"B"}]}}`, w.Body.String())

	assert.False(t, al.answerOrganizationChoice(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/api/v1/login?organization=b", nil).WithContext(ctx), "alice"), "an organization named")
	f.orgs = f.orgs[:1]
	assert.False(t, al.answerOrganizationChoice(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/api/v1/login", nil).WithContext(ctx), "alice"), "one organization: as before")
	assert.False(t, (&APIListener{}).answerOrganizationChoice(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/api/v1/login", nil), "alice"), "standalone")
}
