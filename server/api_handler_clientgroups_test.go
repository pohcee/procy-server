package chserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/db/sqlite"
	"github.com/pohcee/procy-server/server/cgroups"
	"github.com/pohcee/procy-server/server/routes"
	"github.com/pohcee/procy-server/share/tenant"
	"github.com/pohcee/procy-server/share/types"
)

func TestValidateInputClientGroup(t *testing.T) {
	testCases := []struct {
		name    string
		groupID string
		wantErr error
	}{
		{
			name:    "empty group ID",
			groupID: "",
			wantErr: errors.New("group ID cannot be empty"),
		},
		{
			name:    "group ID only with whitespaces",
			groupID: " ",
			wantErr: errors.New("group ID cannot be empty"),
		},
		{
			name:    "group ID with invalid char '?'",
			groupID: "?",
			wantErr: errors.New(`invalid group ID "?": can contain only "A-Za-z0-9_-*"`),
		},
		{
			name:    "group ID with invalid char '.'",
			groupID: "2.1",
			wantErr: errors.New(`invalid group ID "2.1": can contain only "A-Za-z0-9_-*"`),
		},
		{
			name:    "group ID with extra whitespaces",
			groupID: " id ",
			wantErr: errors.New(`invalid group ID " id ": can contain only "A-Za-z0-9_-*"`),
		},
		{
			name:    "group ID with invalid char '/'",
			groupID: "2/1",
			wantErr: errors.New(`invalid group ID "2/1": can contain only "A-Za-z0-9_-*"`),
		},
		{
			name:    "valid group ID with all available chars",
			groupID: "*abc-XYZ_09_ABC-xyz*",
			wantErr: nil,
		},
		{
			name:    "valid group ID with one char",
			groupID: "a",
			wantErr: nil,
		},
		{
			name:    "valid group ID with one char '*'",
			groupID: "*",
			wantErr: nil,
		},
		{
			name:    "valid group ID with max number of chars",
			groupID: "012345678901234567890123456789",
			wantErr: nil,
		},
		{
			name:    "invalid group ID with too many chars",
			groupID: "0123456789012345678901234567890",
			wantErr: errors.New("invalid group ID: max length 30, got 31"),
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// when
			gotErr := validateInputClientGroup(cgroups.ClientGroup{ID: tc.groupID})

			// then
			assert.Equal(t, tc.wantErr, gotErr)
		})
	}
}

func TestValidateInputClientGroupParamsTag(t *testing.T) {
	testCases := []struct {
		name     string
		jsonData *json.RawMessage
		wantRes  bool
		wantErr  error
	}{
		{
			name:     "Tags json parsing ok 1",
			jsonData: jsonData(` ["Linux", "Datacenter 3"] `),
			wantErr:  nil,
		},
		{
			name:     "Tags json parsing ok 2",
			jsonData: jsonData(` { "AND": [ "Linux", "Datacenter 3" ] } `),
			wantErr:  nil,
		},
		{
			name:     "Tags json parsing ok 2",
			jsonData: jsonData(` { "amd": [ "Linux", "Datacenter 3" ] } `),
			wantErr:  errors.New("error, only and/or is allowed for tags group definitions"),
		},
		{
			name:     "Tags json parsing ok 2",
			jsonData: jsonData(` { "": [ "Linux", "Datacenter 3" ] } `),
			wantErr:  errors.New("error, only and/or is allowed for tags group definitions"),
		},
		{
			name:     "Tags json parsing error 1",
			jsonData: jsonData(` [Linux", "Datacenter 3"] `),
			wantErr:  errors.New("error parsing tags group definitions"),
		},
		{
			name:     "Tags json parsing error 2",
			jsonData: jsonData(` { "and": ["T*", "Datacenter 2, "Datacenter 5"] } `),
			wantErr:  errors.New("error parsing tags group definitions"),
		},
		{
			name:     "both and and or are present",
			jsonData: jsonData(` { [ { "and": [ "Linux", "Datacenter 3" ] }, { "or": [ "Linux", "Datacenter 3" ] }    ] } `),
			wantErr:  errors.New("error parsing tags group definitions"),
		},
		{
			name:     "no map keys",
			jsonData: jsonData(` `),
			wantErr:  errors.New("error parsing tags group definitions"),
		},
		{
			name:     "no map keys",
			jsonData: jsonData(` { } `),
			wantErr:  errors.New("error parsing tags group definitions"),
		},
		{
			name:     "no map keys 2 ",
			jsonData: jsonData(` { [] } `),
			wantErr:  errors.New("error parsing tags group definitions"),
		},
		{
			name:     "no map keys 3 ",
			jsonData: jsonData(` [] `),
			wantErr:  errors.New("error parsing tags group definitions"),
		},
		{
			name:     "no map keys 4 ",
			jsonData: jsonData(` { "and" : [] } `),
			wantErr:  errors.New("error parsing tags group definitions"),
		},
		{
			name:     "no tags",
			jsonData: nil,
			wantErr:  nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var group cgroups.ClientGroup
			group.ID = "testg1"
			group.Params = &cgroups.ClientParams{
				Tag: tc.jsonData,
			}

			gotErr := validateInputClientGroup(group)
			assert.Equal(t, tc.wantErr, gotErr)
		})
	}
}

func jsonData(data string) *json.RawMessage {
	bytes := []byte(data)
	return (*json.RawMessage)(&bytes)
}

func putClientGroup(t *testing.T, al *APIListener, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/client-groups/"+id, strings.NewReader(body))
	req = req.WithContext(tenant.WithID(req.Context(), tenant.Default))
	req = mux.SetURLVars(req, map[string]string{routes.ParamGroupID: id})
	w := httptest.NewRecorder()
	al.handlePutClientGroup(w, req)
	return w
}

func TestPutClientGroupParams(t *testing.T) {
	stored := &cgroups.ClientParams{ClientID: &cgroups.ParamValues{"web-*"}}
	testCases := []struct {
		name       string
		body       string
		wantParams *cgroups.ClientParams
	}{
		{
			name:       "params omitted keeps stored params",
			body:       `{"id":"g1","description":"new","allowed_user_groups":["ops"]}`,
			wantParams: stored,
		},
		{
			name:       "params null keeps stored params",
			body:       `{"id":"g1","description":"new","params":null,"allowed_user_groups":["ops"]}`,
			wantParams: stored,
		},
		{
			name:       "empty params clear stored params",
			body:       `{"id":"g1","description":"new","params":{},"allowed_user_groups":["ops"]}`,
			wantParams: &cgroups.ClientParams{},
		},
		{
			name:       "params given replace stored params",
			body:       `{"id":"g1","description":"new","params":{"os_kernel":["linux"]},"allowed_user_groups":["ops"]}`,
			wantParams: &cgroups.ClientParams{OSKernel: &cgroups.ParamValues{"linux"}},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tenant.WithID(context.Background(), tenant.Default)
			gp := makeGroupsProvider(t, sqlite.DataSourceOptions{})
			require.NoError(t, gp.Create(ctx, &cgroups.ClientGroup{ID: "g1", Description: "old", Params: stored, AllowedUserGroups: []string{}}))
			al := &APIListener{apiServices: &apiServices{clientGroupProvider: gp}, Logger: testLog}

			w := putClientGroup(t, al, "g1", tc.body)
			require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())

			got, err := gp.Get(ctx, "g1")
			require.NoError(t, err)
			assert.Equal(t, "new", got.Description)
			assert.Equal(t, types.StringSlice{"ops"}, got.AllowedUserGroups)
			assert.Equal(t, tc.wantParams, got.Params)
		})
	}
}

func TestPutClientGroupWithoutParamsCreatesEmptyGroup(t *testing.T) {
	ctx := tenant.WithID(context.Background(), tenant.Default)
	gp := makeGroupsProvider(t, sqlite.DataSourceOptions{})
	al := &APIListener{apiServices: &apiServices{clientGroupProvider: gp}, Logger: testLog}

	w := putClientGroup(t, al, "g2", `{"id":"g2","description":"d"}`)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())

	got, err := gp.Get(ctx, "g2")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, &cgroups.ClientParams{}, got.Params)
}

func TestPostClientGroupWithoutParams(t *testing.T) {
	ctx := tenant.WithID(context.Background(), tenant.Default)
	gp := makeGroupsProvider(t, sqlite.DataSourceOptions{})
	al := &APIListener{apiServices: &apiServices{clientGroupProvider: gp}, Logger: testLog}

	req := httptest.NewRequest(http.MethodPost, "/client-groups", strings.NewReader(`{"id":"g3","description":"d"}`))
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	al.handlePostClientGroups(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	got, err := gp.Get(ctx, "g3")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, &cgroups.ClientParams{}, got.Params)
}
