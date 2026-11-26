package chserver

import (
	"net/http"

	"github.com/gorilla/mux"

	"github.com/pohcee/procy-server/server/api"
	"github.com/pohcee/procy-server/server/api/users"
	"github.com/pohcee/procy-server/server/auditlog"
)

func (al *APIListener) handleListUserGroups(w http.ResponseWriter, req *http.Request) {
	items, err := al.userService.ListGroups(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(items))
}

func (al *APIListener) handleGetUserGroup(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	name := vars["group_name"]

	group, err := al.userService.GetGroup(req.Context(), name)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(group))
}

func (al *APIListener) handleUpdateUserGroup(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	name := vars["group_name"]

	var input users.Group
	err := parseRequestBody(req.Body, &input)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	group, err := al.userService.UpdateGroup(req.Context(), name, input)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	al.auditLog.Entry(auditlog.ApplicationAuthUserGroup, auditlog.ActionUpdate).
		WithHTTPRequest(req).
		WithRequest(input).
		WithResponse(group).
		WithID(name).
		Save()

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(group))
}

func (al *APIListener) handleDeleteUserGroup(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	name := vars["group_name"]

	err := al.userService.DeleteGroup(req.Context(), name)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	al.auditLog.Entry(auditlog.ApplicationAuthUserGroup, auditlog.ActionDelete).
		WithHTTPRequest(req).
		WithID(name).
		Save()

	w.WriteHeader(http.StatusNoContent)
}
