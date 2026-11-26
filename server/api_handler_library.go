package chserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/gorilla/mux"

	"github.com/pohcee/procy-server/server/api"
	"github.com/pohcee/procy-server/server/api/command"
	errors2 "github.com/pohcee/procy-server/server/api/errors"
	"github.com/pohcee/procy-server/server/auditlog"
	"github.com/pohcee/procy-server/server/library"
	"github.com/pohcee/procy-server/server/routes"
	"github.com/pohcee/procy-server/server/script"
	"github.com/pohcee/procy-server/share/query"
)

const (
	scriptsSaveMessage  = "Saved through /library/scripts"
	commandsSaveMessage = "Saved through /library/commands"
)

var (
	scriptSortAndFilters = map[string]bool{
		"id": true, "name": true, "created_by": true, "created_at": true, "updated_by": true, "updated_at": true,
		"interpreter": true, "is_sudo": true, "cwd": true, "script": true, "tags": true,
	}
	scriptFields = map[string]map[string]bool{
		"scripts": {
			"id": true, "name": true, "created_by": true, "created_at": true, "updated_by": true, "updated_at": true,
			"interpreter": true, "is_sudo": true, "cwd": true, "script": true, "tags": true, "timeout_sec": true,
		},
	}
	commandSortAndFilters = map[string]bool{
		"id": true, "name": true, "created_by": true, "created_at": true, "updated_by": true, "updated_at": true,
		"cmd": true, "tags": true,
	}
	commandFields = map[string]map[string]bool{
		"commands": {
			"id": true, "name": true, "created_by": true, "created_at": true, "updated_by": true, "updated_at": true,
			"cmd": true, "tags": true,
		},
	}
)

// sortViews orders views by the requested columns (JSON field names).
func sortViews[T any](views []T, sorts []query.SortOption) error {
	if len(sorts) == 0 {
		return nil
	}
	maps := make([]map[string]interface{}, len(views))
	for i := range views {
		b, err := json.Marshal(views[i])
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &maps[i]); err != nil {
			return err
		}
	}
	idx := make([]int, len(views))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		for _, so := range sorts {
			x, y := maps[idx[a]][so.Column], maps[idx[b]][so.Column]
			var less, greater bool
			xf, xok := x.(float64)
			yf, yok := y.(float64)
			if xok && yok {
				less, greater = xf < yf, xf > yf
			} else {
				xs, ys := fmt.Sprint(x), fmt.Sprint(y)
				less, greater = xs < ys, xs > ys
			}
			if less || greater {
				return less == so.IsASC
			}
		}
		return false
	})
	sorted := make([]T, len(views))
	for i, j := range idx {
		sorted[i] = views[j]
	}
	copy(views, sorted)
	return nil
}

// listLibraryView lists the entries of one kind through a view, honoring the
// sort, filter, fields and pagination query params.
func listLibraryView[T any](
	al *APIListener, req *http.Request, kind library.Kind, resource string,
	sortAndFilters map[string]bool, supportedFields map[string]map[string]bool,
	toView func(library.Entry) T, prune func(T, map[string]bool) T,
) ([]T, int, error) {
	opts := query.GetListOptions(req)
	err := query.ValidateListOptions(opts, sortAndFilters, sortAndFilters, supportedFields, &query.PaginationConfig{
		MaxLimit:     100,
		DefaultLimit: 20,
	})
	if err != nil {
		return nil, 0, err
	}

	entries, err := al.libraryManager.ListViews(req.Context(), kind)
	if err != nil {
		return nil, 0, err
	}

	serveVars := al.libraryServeVars()
	views := make([]T, 0, len(entries))
	for _, e := range entries {
		v := toView(library.ExpandBuiltin(e, serveVars))
		matches, err := query.MatchesFilters(v, opts.Filters)
		if err != nil {
			return nil, 0, err
		}
		if matches {
			views = append(views, v)
		}
	}
	if err := sortViews(views, opts.Sorts); err != nil {
		return nil, 0, err
	}

	total := len(views)
	start, end := opts.Pagination.GetStartEnd(total)
	page := views[start:end]
	fields := query.RequestedFields(opts.Fields, resource)
	for i := range page {
		page[i] = prune(page[i], fields)
	}
	return page, total, nil
}

// libraryViewEntry loads the live entry for id when it belongs to the view of
// the given kind. found is false when it doesn't exist there.
func (al *APIListener) libraryViewEntry(req *http.Request, id string, kind library.Kind) (e *library.Entry, found bool, err error) {
	e, err = al.libraryManager.GetView(req.Context(), id, kind)
	var apiErr errors2.APIError
	if errors.As(err, &apiErr) && apiErr.HTTPStatus == http.StatusNotFound {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	expanded := library.ExpandBuiltin(*e, al.libraryServeVars())
	return &expanded, true, nil
}

func (al *APIListener) handleListScripts(w http.ResponseWriter, req *http.Request) {
	items, count, err := listLibraryView(al, req, library.KindScript, "scripts", scriptSortAndFilters, scriptFields,
		library.ToScript, library.PruneScript)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	al.writeJSONResponse(w, http.StatusOK, &api.SuccessPayload{
		Data: items,
		Meta: api.NewMeta(count),
	})
}

func (al *APIListener) handleScriptCreate(w http.ResponseWriter, req *http.Request) {
	var scriptInput script.InputScript
	err := parseRequestBody(req.Body, &scriptInput)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	curUsername := api.GetUser(req.Context(), al.Logger)
	if curUsername == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if err := script.Validate(&scriptInput); err != nil {
		al.jsonError(w, err)
		return
	}
	entry, err := al.libraryManager.Create(req.Context(), library.ContentFromScript(scriptInput, nil), scriptsSaveMessage, curUsername)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	storedValue := library.ToScript(*entry)

	al.auditLog.Entry(auditlog.ApplicationLibraryScript, auditlog.ActionCreate).
		WithHTTPRequest(req).
		WithRequest(scriptInput).
		WithResponse(storedValue).
		WithID(storedValue.ID).
		Save()

	al.writeJSONResponse(w, http.StatusCreated, api.NewSuccessPayload(storedValue))
}

func (al *APIListener) handleScriptUpdate(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	idStr, ok := vars[routes.ParamScriptValueID]
	if !ok {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "Script ID is not provided")
		return
	}

	curUsername := api.GetUser(req.Context(), al.Logger)
	if curUsername == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var scriptInput script.InputScript
	err := parseRequestBody(req.Body, &scriptInput)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if err := script.Validate(&scriptInput); err != nil {
		al.jsonError(w, err)
		return
	}

	current, found, err := al.libraryViewEntry(req, idStr, library.KindScript)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if !found {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, "cannot find entry by the provided ID")
		return
	}

	entry, err := al.libraryManager.Save(req.Context(), idStr, library.SaveInput{
		Content:     library.ContentFromScript(scriptInput, current),
		BaseVersion: current.ActiveVersion,
		Message:     scriptsSaveMessage,
	}, curUsername)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	storedValue := library.ToScript(*entry)

	al.auditLog.Entry(auditlog.ApplicationLibraryScript, auditlog.ActionUpdate).
		WithHTTPRequest(req).
		WithRequest(scriptInput).
		WithResponse(storedValue).
		WithID(idStr).
		Save()

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(storedValue))
}

func (al *APIListener) handleReadScript(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	idStr := vars[routes.ParamScriptValueID]
	if idStr == "" {
		al.jsonError(w, errors2.APIError{
			Err:        errors.New("empty script id provided"),
			HTTPStatus: http.StatusBadRequest,
		})
		return
	}

	retrieveOptions := query.GetRetrieveOptions(req)
	if err := query.ValidateRetrieveOptions(retrieveOptions, scriptFields); err != nil {
		al.jsonError(w, err)
		return
	}

	entry, found, err := al.libraryViewEntry(req, idStr, library.KindScript)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if !found {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, fmt.Sprintf("Cannot find a script by the provided id: %s", idStr))
		return
	}

	foundScript := library.PruneScript(library.ToScript(*entry), query.RequestedFields(retrieveOptions.Fields, "scripts"))
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(foundScript))
}

func (al *APIListener) handleDeleteScript(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	idStr := vars[routes.ParamScriptValueID]
	if idStr == "" {
		al.jsonError(w, errors2.APIError{
			Err:        errors.New("empty script id provided"),
			HTTPStatus: http.StatusBadRequest,
		})
		return
	}

	_, found, err := al.libraryViewEntry(req, idStr, library.KindScript)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if !found {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, "cannot find this entry by the provided id")
		return
	}
	if err := al.libraryManager.Delete(req.Context(), idStr, api.GetUser(req.Context(), al.Logger)); err != nil {
		al.jsonError(w, err)
		return
	}

	al.auditLog.Entry(auditlog.ApplicationLibraryScript, auditlog.ActionDelete).
		WithHTTPRequest(req).
		WithID(idStr).
		Save()

	w.WriteHeader(http.StatusNoContent)
}

func (al *APIListener) handleListCommands(w http.ResponseWriter, req *http.Request) {
	items, count, err := listLibraryView(al, req, library.KindCommand, "commands", commandSortAndFilters, commandFields,
		library.ToCommand, library.PruneCommand)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	al.writeJSONResponse(w, http.StatusOK, &api.SuccessPayload{
		Data: items,
		Meta: api.NewMeta(count),
	})
}

func (al *APIListener) handleCommandCreate(w http.ResponseWriter, req *http.Request) {
	var commandInput command.InputCommand
	err := parseRequestBody(req.Body, &commandInput)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	curUsername := api.GetUser(req.Context(), al.Logger)
	if curUsername == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if err := command.Validate(&commandInput); err != nil {
		al.jsonError(w, err)
		return
	}
	entry, err := al.libraryManager.Create(req.Context(), library.ContentFromCommand(commandInput, nil), commandsSaveMessage, curUsername)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	storedValue := library.ToCommand(*entry)

	al.auditLog.Entry(auditlog.ApplicationLibraryCommand, auditlog.ActionCreate).
		WithHTTPRequest(req).
		WithRequest(commandInput).
		WithResponse(storedValue).
		WithID(storedValue.ID).
		Save()

	al.writeJSONResponse(w, http.StatusCreated, api.NewSuccessPayload(storedValue))
}

func (al *APIListener) handleCommandUpdate(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	idStr, ok := vars[routes.ParamCommandValueID]
	if !ok {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "Command ID is not provided")
		return
	}

	curUsername := api.GetUser(req.Context(), al.Logger)
	if curUsername == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var commandInput command.InputCommand
	err := parseRequestBody(req.Body, &commandInput)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if err := command.Validate(&commandInput); err != nil {
		al.jsonError(w, err)
		return
	}

	current, found, err := al.libraryViewEntry(req, idStr, library.KindCommand)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if !found {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, "cannot find entry by the provided ID")
		return
	}

	entry, err := al.libraryManager.Save(req.Context(), idStr, library.SaveInput{
		Content:     library.ContentFromCommand(commandInput, current),
		BaseVersion: current.ActiveVersion,
		Message:     commandsSaveMessage,
	}, curUsername)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	storedValue := library.ToCommand(*entry)

	al.auditLog.Entry(auditlog.ApplicationLibraryCommand, auditlog.ActionUpdate).
		WithHTTPRequest(req).
		WithRequest(commandInput).
		WithResponse(storedValue).
		WithID(idStr).
		Save()

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(storedValue))
}

func (al *APIListener) handleReadCommand(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	idStr := vars[routes.ParamCommandValueID]
	if idStr == "" {
		al.jsonError(w, errors2.APIError{
			Err:        errors.New("empty command id provided"),
			HTTPStatus: http.StatusBadRequest,
		})
		return
	}

	retrieveOptions := query.GetRetrieveOptions(req)
	if err := query.ValidateRetrieveOptions(retrieveOptions, commandFields); err != nil {
		al.jsonError(w, err)
		return
	}

	entry, found, err := al.libraryViewEntry(req, idStr, library.KindCommand)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if !found {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, fmt.Sprintf("Cannot find a command by the provided id: %s", idStr))
		return
	}

	foundCommand := library.PruneCommand(library.ToCommand(*entry), query.RequestedFields(retrieveOptions.Fields, "commands"))
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(foundCommand))
}

func (al *APIListener) handleDeleteCommand(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	idStr := vars[routes.ParamCommandValueID]
	if idStr == "" {
		al.jsonError(w, errors2.APIError{
			Err:        errors.New("empty command id provided"),
			HTTPStatus: http.StatusBadRequest,
		})
		return
	}

	_, found, err := al.libraryViewEntry(req, idStr, library.KindCommand)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if !found {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, "cannot find this entry by the provided id")
		return
	}
	if err := al.libraryManager.Delete(req.Context(), idStr, api.GetUser(req.Context(), al.Logger)); err != nil {
		al.jsonError(w, err)
		return
	}

	al.auditLog.Entry(auditlog.ApplicationLibraryCommand, auditlog.ActionDelete).
		WithHTTPRequest(req).
		WithID(idStr).
		Save()

	w.WriteHeader(http.StatusNoContent)
}
