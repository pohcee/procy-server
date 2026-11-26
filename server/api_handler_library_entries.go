package chserver

import (
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/gorilla/mux"

	"github.com/pohcee/procy-server/server/api"
	errors2 "github.com/pohcee/procy-server/server/api/errors"
	"github.com/pohcee/procy-server/server/api/jobs"
	"github.com/pohcee/procy-server/server/auditlog"
	"github.com/pohcee/procy-server/server/library"
	"github.com/pohcee/procy-server/server/routes"
)

const (
	entriesPath   = "/library/entries"
	entryPath     = entriesPath + "/{" + routes.ParamEntryID + "}"
	entryVersions = entryPath + "/versions"
	entryVersionN = entryVersions + "/{" + routes.ParamEntryVersion + "}"
)

// addLibraryEntriesRoutes registers the /library/entries endpoints on r.
func (al *APIListener) addLibraryEntriesRoutes(r *mux.Router) {
	r.HandleFunc(entriesPath, al.handleListLibraryEntries).Methods(http.MethodGet)
	r.HandleFunc(entriesPath, al.handleCreateLibraryEntry).Methods(http.MethodPost).Name(routes.LibraryEntryWriteRouteName)
	r.HandleFunc(entryPath, al.handleGetLibraryEntry).Methods(http.MethodGet)
	r.HandleFunc(entryPath, al.handleSaveLibraryEntry).Methods(http.MethodPut).Name(routes.LibraryEntryWriteRouteName + "-put")
	r.HandleFunc(entryPath, al.handleDeleteLibraryEntry).Methods(http.MethodDelete)
	r.HandleFunc(entryPath+"/used-by", al.handleLibraryEntryUsedBy).Methods(http.MethodGet)
	r.HandleFunc(entryPath+"/undelete", al.handleUndeleteLibraryEntry).Methods(http.MethodPost)
	r.HandleFunc(entryPath+"/run", al.handleRunLibraryEntry).Methods(http.MethodPost)
	r.HandleFunc(entryVersions, al.handleListLibraryEntryVersions).Methods(http.MethodGet)
	r.HandleFunc(entryVersionN, al.handleGetLibraryEntryVersion).Methods(http.MethodGet)
	r.HandleFunc(entryVersionN+"/restore", al.handleRestoreLibraryEntryVersion).Methods(http.MethodPost).Name(routes.LibraryEntryWriteRouteName + "-restore")
}

func badRequest(msg string) error {
	return errors2.APIError{Message: msg, Err: errors.New(msg), HTTPStatus: http.StatusBadRequest}
}

// libraryEntryUser returns the calling user or writes a 401.
func (al *APIListener) libraryEntryUser(w http.ResponseWriter, req *http.Request) (string, bool) {
	user := api.GetUser(req.Context(), al.Logger)
	if user == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusUnauthorized, "unauthorized")
		return "", false
	}
	return user, true
}

func boolParam(req *http.Request, name string) (*bool, error) {
	raw := req.URL.Query().Get(name)
	if raw == "" {
		return nil, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return nil, badRequest("invalid value for " + name + ": expected true or false")
	}
	return &b, nil
}

func intParam(req *http.Request, name string) (int, error) {
	raw := req.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, badRequest("invalid value for " + name)
	}
	return n, nil
}

func (al *APIListener) auditLibraryEntry(req *http.Request, action, id string, request, response interface{}) {
	e := al.auditLog.Entry(auditlog.ApplicationLibraryEntry, action).WithHTTPRequest(req).WithID(id)
	if request != nil {
		e = e.WithRequest(request)
	}
	if response != nil {
		e = e.WithResponse(response)
	}
	e.Save()
}

func (al *APIListener) handleListLibraryEntries(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	opts := library.ListOptions{
		Name: q.Get("filter[name]"),
		Tag:  q.Get("filter[tags]"),
		OS:   q.Get("filter[os]"),
		Sort: q.Get("sort"),
	}
	var err error
	if opts.Builtin, err = boolParam(req, "filter[builtin]"); err != nil {
		al.jsonError(w, err)
		return
	}
	if opts.Deleted, err = boolParam(req, "filter[deleted]"); err != nil {
		al.jsonError(w, err)
		return
	}
	if opts.Sort != "" && opts.Sort != "name" && opts.Sort != "-updated_at" {
		al.jsonError(w, badRequest("sort must be name or -updated_at"))
		return
	}
	if opts.Offset, err = intParam(req, "page[offset]"); err != nil {
		al.jsonError(w, err)
		return
	}
	if opts.Limit, err = intParam(req, "page[limit]"); err != nil {
		al.jsonError(w, err)
		return
	}
	if opts.OS != "" && !slices.Contains(library.AllOS, opts.OS) {
		al.jsonError(w, badRequest("filter[os] has unsupported value "+strconv.Quote(opts.OS)+": use linux, windows or darwin"))
		return
	}

	entries, count, err := al.libraryManager.List(req.Context(), opts)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	vars := al.libraryServeVars()
	for i := range entries {
		entries[i] = library.ExpandBuiltin(entries[i], vars)
	}
	al.writeJSONResponse(w, http.StatusOK, &api.SuccessPayload{Data: entries, Meta: api.NewMeta(count)})
}

type libraryEntryCreateInput struct {
	library.Content
	Message string `json:"message"`
}

func (al *APIListener) handleCreateLibraryEntry(w http.ResponseWriter, req *http.Request) {
	user, ok := al.libraryEntryUser(w, req)
	if !ok {
		return
	}
	var in libraryEntryCreateInput
	if err := parseRequestBody(req.Body, &in); err != nil {
		al.jsonError(w, err)
		return
	}
	entry, err := al.libraryManager.Create(req.Context(), in.Content, in.Message, user)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	al.auditLibraryEntry(req, auditlog.ActionCreate, entry.ID, in, entry)
	al.writeJSONResponse(w, http.StatusCreated, api.NewSuccessPayload(entry))
}

func (al *APIListener) handleGetLibraryEntry(w http.ResponseWriter, req *http.Request) {
	includeDeleted, err := boolParam(req, "include_deleted")
	if err != nil {
		al.jsonError(w, err)
		return
	}
	entry, err := al.libraryManager.Get(req.Context(), mux.Vars(req)[routes.ParamEntryID], includeDeleted != nil && *includeDeleted)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(library.ExpandBuiltin(*entry, al.libraryServeVars())))
}

// libraryEntrySaveInput is library.SaveInput with base_version detectable as missing.
type libraryEntrySaveInput struct {
	library.Content
	BaseVersion *int   `json:"base_version"`
	Message     string `json:"message"`
}

func (al *APIListener) handleSaveLibraryEntry(w http.ResponseWriter, req *http.Request) {
	user, ok := al.libraryEntryUser(w, req)
	if !ok {
		return
	}
	var in libraryEntrySaveInput
	if err := parseRequestBody(req.Body, &in); err != nil {
		al.jsonError(w, err)
		return
	}
	if in.BaseVersion == nil {
		al.jsonError(w, badRequest("base_version is required"))
		return
	}
	id := mux.Vars(req)[routes.ParamEntryID]
	entry, err := al.libraryManager.Save(req.Context(), id, library.SaveInput{
		Content:     in.Content,
		BaseVersion: *in.BaseVersion,
		Message:     in.Message,
	}, user)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	al.auditLibraryEntry(req, auditlog.ActionUpdate, id, in, entry)
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(entry))
}

func (al *APIListener) handleDeleteLibraryEntry(w http.ResponseWriter, req *http.Request) {
	user, ok := al.libraryEntryUser(w, req)
	if !ok {
		return
	}
	id := mux.Vars(req)[routes.ParamEntryID]
	if err := al.libraryManager.Delete(req.Context(), id, user); err != nil {
		al.jsonError(w, err)
		return
	}
	al.auditLibraryEntry(req, auditlog.ActionDelete, id, nil, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (al *APIListener) handleUndeleteLibraryEntry(w http.ResponseWriter, req *http.Request) {
	user, ok := al.libraryEntryUser(w, req)
	if !ok {
		return
	}
	id := mux.Vars(req)[routes.ParamEntryID]
	entry, err := al.libraryManager.Undelete(req.Context(), id, user)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	al.auditLibraryEntry(req, auditlog.ActionUndelete, id, nil, entry)
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(entry))
}

func (al *APIListener) handleListLibraryEntryVersions(w http.ResponseWriter, req *http.Request) {
	versions, err := al.libraryManager.Versions(req.Context(), mux.Vars(req)[routes.ParamEntryID])
	if err != nil {
		al.jsonError(w, err)
		return
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(versions))
}

func versionParam(req *http.Request) (int, error) {
	n, err := strconv.Atoi(mux.Vars(req)[routes.ParamEntryVersion])
	if err != nil || n < 1 {
		return 0, badRequest("version must be a positive integer")
	}
	return n, nil
}

func (al *APIListener) handleGetLibraryEntryVersion(w http.ResponseWriter, req *http.Request) {
	n, err := versionParam(req)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	v, err := al.libraryManager.Version(req.Context(), mux.Vars(req)[routes.ParamEntryID], n)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if vars := al.libraryServeVars(); vars != nil && v.Content != nil {
		// versions carry no builtin flag: look at the entry
		entry, err := al.libraryManager.Get(req.Context(), mux.Vars(req)[routes.ParamEntryID], true)
		if err != nil {
			al.jsonError(w, err)
			return
		}
		if entry.BuiltinKey != nil {
			c := *v.Content
			c.Implementations = library.ExpandImplementations(c.Implementations, vars)
			v.Content = &c
		}
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(v))
}

func (al *APIListener) handleRestoreLibraryEntryVersion(w http.ResponseWriter, req *http.Request) {
	user, ok := al.libraryEntryUser(w, req)
	if !ok {
		return
	}
	n, err := versionParam(req)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	var in struct {
		BaseVersion *int `json:"base_version"`
	}
	if err := parseRequestBody(req.Body, &in); err != nil {
		al.jsonError(w, err)
		return
	}
	if in.BaseVersion == nil {
		al.jsonError(w, badRequest("base_version is required"))
		return
	}
	id := mux.Vars(req)[routes.ParamEntryID]
	entry, err := al.libraryManager.Restore(req.Context(), id, n, *in.BaseVersion, user)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	al.auditLibraryEntry(req, auditlog.ActionRestore, id, in, entry)
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(entry))
}

// handleRunLibraryEntry handles POST /library/entries/{entry_id}/run
func (al *APIListener) handleRunLibraryEntry(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	id := mux.Vars(req)[routes.ParamEntryID]

	var reqBody jobs.MultiJobRequest
	if err := parseRequestBody(req.Body, &reqBody); err != nil {
		al.jsonError(w, err)
		return
	}

	// 404 before anything about the targets
	if _, err := al.libraryManager.ActiveForRun(ctx, id); err != nil {
		al.jsonError(w, err)
		return
	}

	orderedClients, _, err := al.getOrderedClientsWithValidation(ctx, &reqBody)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	reqBody.OrderedClients = orderedClients

	curUser, err := al.getUserModelForAuth(ctx)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	clientGroups, err := al.clientGroupProvider.GetAll(ctx)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if err := al.clientService.CheckClientsAccess(reqBody.OrderedClients, curUser, clientGroups); err != nil {
		al.jsonError(w, err)
		return
	}
	reqBody.Username = curUser.Username

	multiJob, err := al.jobRunner.StartLibraryMultiJob(ctx, id, &reqBody)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	resp := newJobResponse{JID: multiJob.JID}
	al.auditLog.Entry(auditlog.ApplicationLibraryEntry, auditlog.ActionRun).
		WithHTTPRequest(req).
		WithRequest(libraryRunWSRequest{EntryID: id, MultiJobRequest: reqBody}).
		WithResponse(resp).
		WithID(multiJob.JID).
		SaveForMultipleClients(reqBody.OrderedClients)

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(resp))
}

func (al *APIListener) handleLibraryEntryUsedBy(w http.ResponseWriter, req *http.Request) {
	refs, err := al.libraryManager.UsedBy(req.Context(), mux.Vars(req)[routes.ParamEntryID])
	if err != nil {
		al.jsonError(w, err)
		return
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(refs))
}
