package chserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gorilla/mux"

	"github.com/openrport/openrport/server/api"
	errors2 "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/auditlog"
	"github.com/openrport/openrport/server/routes"
	"github.com/openrport/openrport/server/validation"
	"github.com/openrport/openrport/share/comm"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/ws"
)

type jobPayload struct {
	JID         *string     `json:"jid,omitempty"`
	Status      *string     `json:"status,omitempty"`
	FinishedAt  **time.Time `json:"finished_at,omitempty"`
	ClientID    *string     `json:"client_id,omitempty"`
	ClientName  *string     `json:"client_name,omitempty"`
	Command     *string     `json:"command,omitempty"`
	Cwd         *string     `json:"cwd,omitempty"`
	Interpreter *string     `json:"interpreter,omitempty"`
	PID         **int       `json:"pid,omitempty"`
	StartedAt   *time.Time  `json:"started_at,omitempty"`
	CreatedBy   *string     `json:"created_by,omitempty"`
	TimeoutSec  *int        `json:"timeout_sec,omitempty"`
	MultiJobID  **string    `json:"multi_job_id,omitempty"`
	ScheduleID  **string    `json:"schedule_id,omitempty"`
	Error       *string     `json:"error,omitempty"`
	Result      **jobResult `json:"result,omitempty"`
	IsSudo      *bool       `json:"is_sudo,omitempty"`
	IsScript    *bool       `json:"is_script,omitempty"`

	// Library runs only: left out for other jobs even when requested.
	LibraryEntryID *string `json:"library_entry_id,omitempty"`
	LibraryVersion *int    `json:"library_version,omitempty"`
	OS             *string `json:"os,omitempty"`
}

type jobResult struct {
	StdOut  *string `json:"stdout,omitempty"`
	StdErr  *string `json:"stderr,omitempty"`
	Summary *string `json:"summary,omitempty"`
}

func convertToJobsPayload(jobs []*models.Job, fields []query.FieldsOption) []jobPayload {
	requestedFields := query.RequestedFields(fields, "jobs")
	if len(requestedFields) == 0 {
		requestedFields = query.RequestedFields(fields, "commands")
		if len(requestedFields) == 0 {
			requestedFields = query.RequestedFields(fields, "scripts")
		}
	}
	requestedResultFields := query.RequestedFields(fields, "result")

	result := make([]jobPayload, len(jobs))
	for i, job := range jobs {
		if requestedFields["jid"] {
			result[i].JID = &job.JID
		}
		if requestedFields["status"] {
			result[i].Status = &job.Status
		}
		if requestedFields["finished_at"] {
			result[i].FinishedAt = &job.FinishedAt
		}
		if requestedFields["client_id"] {
			result[i].ClientID = &job.ClientID
		}
		if requestedFields["client_name"] {
			result[i].ClientName = &job.ClientName
		}
		if requestedFields["command"] {
			result[i].Command = &job.Command
		}
		if requestedFields["cwd"] {
			result[i].Cwd = &job.Cwd
		}
		if requestedFields["interpreter"] {
			result[i].Interpreter = &job.Interpreter
		}
		if requestedFields["pid"] {
			result[i].PID = &job.PID
		}
		if requestedFields["started_at"] {
			result[i].StartedAt = &job.StartedAt
		}
		if requestedFields["created_by"] {
			result[i].CreatedBy = &job.CreatedBy
		}
		if requestedFields["timeout_sec"] {
			result[i].TimeoutSec = &job.TimeoutSec
		}
		if requestedFields["multi_job_id"] {
			result[i].MultiJobID = &job.MultiJobID
		}
		if requestedFields["schedule_id"] {
			result[i].ScheduleID = &job.ScheduleID
		}
		if requestedFields["error"] {
			result[i].Error = &job.Error
		}
		if requestedFields["is_sudo"] {
			result[i].IsSudo = &job.IsSudo
		}
		if requestedFields["is_script"] {
			result[i].IsScript = &job.IsScript
		}
		if requestedFields["library_entry_id"] && job.LibraryEntryID != "" {
			result[i].LibraryEntryID = &job.LibraryEntryID
		}
		if requestedFields["library_version"] && job.LibraryVersion != 0 {
			result[i].LibraryVersion = &job.LibraryVersion
		}
		if requestedFields["os"] && job.OS != "" {
			result[i].OS = &job.OS
		}
		if len(requestedResultFields) > 0 {
			result[i].Result = new(*jobResult)
			if job.Result != nil {
				(*result[i].Result) = &jobResult{}
				if requestedResultFields["stdout"] {
					(*result[i].Result).StdOut = &job.Result.StdOut
				}
				if requestedResultFields["stderr"] {
					(*result[i].Result).StdErr = &job.Result.StdErr
				}
				if requestedResultFields["summary"] {
					(*result[i].Result).Summary = &job.Result.Summary
				}
			}
		}
	}

	return result
}

type newJobResponse struct {
	JID string `json:"jid"`
}

// handlePostCommand handles POST /clients/{client_id}/commands
func (al *APIListener) handlePostCommand(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	cid := vars[routes.ParamClientID]
	if cid == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("Missing %q client id route param.", routes.ParamClientID))
		return
	}

	execCmdInput := &api.ExecuteInput{}
	err := parseRequestBody(req.Body, &execCmdInput)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	execCmdInput.ClientID = cid
	execCmdInput.IsScript = false

	resp := al.handleExecuteCommand(req.Context(), w, execCmdInput)

	if resp != nil {
		al.auditLog.Entry(auditlog.ApplicationClientCommand, auditlog.ActionExecuteStart).
			WithHTTPRequest(req).
			WithClientID(cid).
			WithRequest(execCmdInput).
			WithResponse(resp).
			WithID(resp.JID).
			Save()
	}
}

// handleGetCommands handles GET /clients/{client_id}/commands
func (al *APIListener) handleGetCommands(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	cid := vars[routes.ParamClientID]
	if cid == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("Missing %q route param.", routes.ParamClientID))
		return
	}

	options := query.NewOptions(req, nil, nil, jobs.JobListDefaultFields)

	err := query.ValidateListOptions(options, jobs.JobSupportedSorts, jobs.JobSupportedFilters, jobs.JobSupportedFields, &query.PaginationConfig{
		MaxLimit:     jobs.MaxLimit,
		DefaultLimit: jobs.DefaultLimit,
	})
	if err != nil {
		al.jsonError(w, err)
		return
	}

	options.Filters = append(options.Filters, query.FilterOption{Column: []string{"client_id"}, Values: []string{cid}})
	result, err := al.jobProvider.List(req.Context(), options)
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to get client jobs: client_id=%q.", cid), err)
		return
	}

	totalCount, err := al.jobProvider.Count(req.Context(), options)
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to get client jobs: client_id=%q.", cid), err)
		return
	}

	payload := &api.SuccessPayload{
		Data: convertToJobsPayload(result, options.Fields),
		Meta: api.NewMeta(totalCount),
	}
	al.writeJSONResponse(w, http.StatusOK, payload)
}

// handleGetMultiClientCommandJobs handles GET /commands/{job_id}/jobs
func (al *APIListener) handleGetMultiClientCommandJobs(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	multiJobID := vars[routes.ParamJobID]
	if multiJobID == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("Missing %q route param.", routes.ParamJobID))
		return
	}

	options := query.NewOptions(req, nil, nil, jobs.JobListDefaultFields)

	err := query.ValidateListOptions(options, jobs.JobSupportedSorts, jobs.JobSupportedFilters, jobs.JobSupportedFields, &query.PaginationConfig{
		MaxLimit:     jobs.MaxLimit,
		DefaultLimit: jobs.DefaultLimit,
	})
	if err != nil {
		al.jsonError(w, err)
		return
	}

	// Same rule as GET /commands/{job_id} (see canSeeMultiJob).
	multiJob, err := al.jobProvider.GetMultiJob(req.Context(), multiJobID)
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to find a multi-client job[id=%q].", multiJobID), err)
		return
	}
	if multiJob != nil {
		curUser, err := al.getUserModelForAuth(req.Context())
		if err != nil {
			al.jsonError(w, err)
			return
		}
		ok, err := al.canSeeMultiJob(req.Context(), curUser, multiJob)
		if err != nil {
			al.jsonError(w, err)
			return
		}
		if !ok {
			al.jsonErrorResponseWithError(w, http.StatusForbidden, "forbidden", fmt.Errorf("you are not allowed to access items created by another user"))
			return
		}
	}

	options.Filters = append(options.Filters, query.FilterOption{Column: []string{"multi_job_id"}, Values: []string{multiJobID}})
	result, err := al.jobProvider.List(req.Context(), options)
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to get jobs: multi_job_id=%q.", multiJobID), err)
		return
	}

	totalCount, err := al.jobProvider.Count(req.Context(), options)
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to get jobs: multi_job_id=%q.", multiJobID), err)
		return
	}

	payload := &api.SuccessPayload{
		Data: convertToJobsPayload(result, options.Fields),
		Meta: api.NewMeta(totalCount),
	}
	al.writeJSONResponse(w, http.StatusOK, payload)
}

// handleGetCommand handles GET /clients/{client_id}/commands/{job_id}
func (al *APIListener) handleGetCommand(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	cid := vars[routes.ParamClientID]
	if cid == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("Missing %q route param.", routes.ParamClientID))
		return
	}
	jid := vars[routes.ParamJobID]
	if jid == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("Missing %q route param.", routes.ParamJobID))
		return
	}

	job, err := al.jobProvider.GetByJID(req.Context(), cid, jid)
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to find a job[id=%q].", jid), err)
		return
	}
	if job == nil {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, fmt.Sprintf("Job[id=%q] not found.", jid))
		return
	}

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(job))
}

// TODO: refactor to reuse similar code for REST API and WebSocket to execute cmds if both will be supported
// handlePostMultiClientCommand handles POST /commands
func (al *APIListener) handlePostMultiClientCommand(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	var reqBody jobs.MultiJobRequest
	err := parseRequestBody(req.Body, &reqBody)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	if reqBody.Command == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "Command cannot be empty.")
		return
	}
	if err := validation.ValidateInterpreter(reqBody.Interpreter, reqBody.IsScript); err != nil {
		al.jsonErrorResponseWithError(w, http.StatusBadRequest, "Invalid interpreter.", err)
		return
	}

	orderedClients, _, responseErr := al.getOrderedClientsWithValidation(ctx, &reqBody)
	if responseErr != nil {
		al.jsonError(w, responseErr)
		return
	}

	reqBody.OrderedClients = orderedClients

	curUser, err := al.getUserModelForAuth(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}

	clientGroups, err := al.clientGroupProvider.GetAll(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}
	err = al.clientService.CheckClientsAccess(reqBody.OrderedClients, curUser, clientGroups)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	reqBody.Username = curUser.Username

	multiJob, err := al.jobRunner.StartMultiClientJob(ctx, &reqBody)
	if err != nil {
		al.jsonError(w, err)
		return
	}

	resp := newJobResponse{
		JID: multiJob.JID,
	}

	al.auditLog.Entry(auditlog.ApplicationClientCommand, auditlog.ActionExecuteStart).
		WithHTTPRequest(req).
		WithRequest(reqBody).
		WithResponse(resp).
		WithID(multiJob.JID).
		SaveForMultipleClients(reqBody.OrderedClients)

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(resp))

	al.Debugf("Multi-client Job[id=%q] created to execute remote command on clients %s, groups %s, tags %s: %q.", multiJob.JID, reqBody.ClientIDs, reqBody.GroupIDs, reqBody.GetClientTags(), reqBody.Command)
}

func (al *APIListener) handleExecuteCommand(ctx context.Context, w http.ResponseWriter, executeInput *api.ExecuteInput) *newJobResponse {
	if executeInput.Command == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, "Command cannot be empty.")
		return nil
	}
	if err := validation.ValidateInterpreter(executeInput.Interpreter, executeInput.IsScript); err != nil {
		al.jsonErrorResponseWithError(w, http.StatusBadRequest, "Invalid interpreter.", err)
		return nil
	}

	if executeInput.TimeoutSec <= 0 {
		executeInput.TimeoutSec = al.config.Server.RunRemoteCmdTimeoutSec
	}

	client, err := al.clientService.GetActiveByID(ctx, executeInput.ClientID)
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to find an active client with id=%q.", executeInput.ClientID), err)
		return nil
	}
	if client == nil {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, fmt.Sprintf("Active client with id=%q not found.", executeInput.ClientID))
		return nil
	}

	if client.IsPaused() {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, fmt.Sprintf("failed to execute command/script for client with id %s due to client being paused (reason = %s)", client.GetID(), client.GetPausedReason()))
		return nil
	}

	// send the command to the client
	// Send a job with all possible info in order to get the full-populated job back (in client-listener) when it's done.
	// Needed when server restarts to get all job data from client. Because on server restart job running info is lost.
	jid, err := generateNewJobID()
	if err != nil {
		al.jsonError(w, err)
		return nil
	}
	curJob := models.Job{
		JID:         jid,
		FinishedAt:  nil,
		ClientID:    executeInput.ClientID,
		ClientName:  client.GetName(),
		Command:     executeInput.Command,
		Interpreter: executeInput.Interpreter,
		CreatedBy:   api.GetUser(ctx, al.Logger),
		TimeoutSec:  executeInput.TimeoutSec,
		Result:      nil,
		Cwd:         executeInput.Cwd,
		IsSudo:      executeInput.IsSudo,
		IsScript:    executeInput.IsScript,
	}
	sshResp, err := al.dispatcher.RunJob(ctx, executeInput.ClientID, &curJob)
	if err != nil {
		if _, ok := err.(*comm.ClientError); ok {
			al.jsonErrorResponseWithTitle(w, http.StatusConflict, err.Error())
		} else {
			al.jsonErrorResponseWithError(w, http.StatusInternalServerError, "Failed to execute remote command.", err)
		}
		return nil
	}

	// set fields received in response
	curJob.PID = &sshResp.Pid
	curJob.StartedAt = sshResp.StartedAt
	curJob.Status = models.JobStatusRunning

	if err := al.jobProvider.CreateJob(ctx, &curJob); err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, "Failed to persist a new job.", err)
		return nil
	}

	resp := &newJobResponse{
		JID: curJob.JID,
	}

	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(resp))

	al.Debugf("Job[id=%q] created to execute remote command on client with id=%q: %q.", curJob.JID, executeInput.ClientID, executeInput.Command)

	return resp
}

// handleCommandsWS handles GET /ws/commands
func (al *APIListener) handleCommandsWS(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	uiConn, err := apiUpgrader.Upgrade(w, req, nil)
	if err != nil {
		al.Errorf("Failed to establish WS connection: %v", err)
		return
	}
	uiConnTS := ws.NewConcurrentWebSocket(uiConn, al.Logger)
	inboundMsg := &jobs.MultiJobRequest{}
	err = uiConnTS.ReadJSON(inboundMsg)
	if err == io.EOF { // is handled separately to return an informative error message
		uiConnTS.WriteError("Inbound message should contain non empty json object with command data.", nil)
		return
	} else if err != nil {
		uiConnTS.WriteError("Invalid JSON data.", err)
		return
	}

	orderedClients, _, responseErr := al.getOrderedClientsWithValidation(ctx, inboundMsg)
	if responseErr != nil {
		uiConnTS.WriteError("", responseErr)
		return
	}

	inboundMsg.OrderedClients = orderedClients
	inboundMsg.IsScript = false

	auditLogEntry := al.auditLog.Entry(auditlog.ApplicationClientCommand, auditlog.ActionExecuteStart).WithHTTPRequest(req)

	al.handleCommandsExecutionWS(ctx, uiConnTS, inboundMsg, auditLogEntry)
}

// handleGetMultiClientCommand handles GET /commands/{job_id}
func (al *APIListener) handleGetMultiClientCommand(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	jid := vars[routes.ParamJobID]
	if jid == "" {
		al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, fmt.Sprintf("Missing %q route param.", routes.ParamJobID))
		return
	}

	job, err := al.jobProvider.GetMultiJob(req.Context(), jid)
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to find a multi-client job[id=%q].", jid), err)
		return
	}
	if job == nil {
		al.jsonErrorResponseWithTitle(w, http.StatusNotFound, fmt.Sprintf("Multi-client Job[id=%q] not found.", jid))
		return
	}

	curUser, err := al.getUserModelForAuth(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}
	ok, err := al.canSeeMultiJob(req.Context(), curUser, job)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if ok {
		al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(job))
		return
	}
	al.jsonErrorResponseWithError(w, http.StatusForbidden, "forbidden", fmt.Errorf("you are not allowed to access items created by another user"))
}

// canSeeMultiJob reports whether user may see the multi-client job mj and its clients' jobs: an admin, its creator,
// or, for a run of a schedule that still exists, a user who may see that schedule (GET /schedules/{id} needs only the
// scheduler permission) and who has access to every device of the run (its results are those devices' output).
func (al *APIListener) canSeeMultiJob(ctx context.Context, user *users.User, mj *models.MultiJob) (bool, error) {
	if user.IsAdmin() || mj.CreatedBy == user.Username {
		return true, nil
	}
	if mj.ScheduleID == nil || *mj.ScheduleID == "" || al.scheduleManager == nil {
		return false, nil
	}
	ok, err := al.hasPermission(ctx, user, users.PermissionScheduler)
	if err != nil || !ok {
		return false, err
	}
	s, err := al.scheduleManager.Get(ctx, *mj.ScheduleID)
	if err != nil || s == nil {
		return false, err
	}
	return al.canAccessClients(ctx, user, mj)
}

// canAccessClients reports whether user has access (client access rules) to every device of mj: its client ids and
// those of its jobs. A device that no longer exists counts as no access.
func (al *APIListener) canAccessClients(ctx context.Context, user *users.User, mj *models.MultiJob) (bool, error) {
	ids := map[string]bool{}
	for _, id := range mj.ClientIDs {
		ids[id] = true
	}
	for _, j := range mj.Jobs {
		ids[j.ClientID] = true
	}
	if len(ids) == 0 {
		return true, nil
	}
	clientGroups, err := al.clientGroupProvider.GetAll(ctx)
	if err != nil {
		return false, err
	}
	for id := range ids {
		err := al.clientService.CheckClientAccess(ctx, id, user, clientGroups)
		if err == nil {
			continue
		}
		var apiErr errors2.APIError
		if errors.As(err, &apiErr) && (apiErr.HTTPStatus == http.StatusForbidden || apiErr.HTTPStatus == http.StatusNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// hasPermission reports whether user holds permission the way permissionsMiddleware decides it: when the user store
// cannot evaluate group permissions only administrators do, otherwise one of the user's groups must grant it.
func (al *APIListener) hasPermission(ctx context.Context, user *users.User, permission string) (bool, error) {
	if !al.userService.SupportsGroupPermissions() {
		return user.IsAdmin(), nil
	}
	err := al.userService.CheckPermission(ctx, user, permission)
	if err == nil {
		return true, nil
	}
	var apiErr errors2.APIError
	if errors.As(err, &apiErr) && apiErr.HTTPStatus == http.StatusForbidden {
		return false, nil
	}
	return false, err
}

// handleGetMultiClientCommands handles GET /commands
func (al *APIListener) handleGetMultiClientCommands(w http.ResponseWriter, req *http.Request) {
	listOptions := query.GetListOptions(req)

	err := query.ValidateListOptions(listOptions, jobs.MultiJobSupportedSorts, jobs.MultiJobSupportedFilters, nil /*fields*/, &query.PaginationConfig{
		MaxLimit:     1000,
		DefaultLimit: 100,
	})
	if err != nil {
		al.jsonError(w, err)
		return
	}

	// Non-admins see the multi-client jobs GET /commands/{job_id} shows them: the ones they created and, with the
	// scheduler permission, the runs of every existing schedule.
	curUser, err := al.getUserModelForAuth(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}
	if !curUser.IsAdmin() {
		scheduler, err := al.hasPermission(req.Context(), curUser, users.PermissionScheduler)
		if err != nil {
			al.jsonError(w, err)
			return
		}
		if scheduler && al.scheduleManager != nil {
			listOptions.Filters = append(listOptions.Filters, jobs.CreatedByOrScheduled(curUser.Username))
		} else {
			listOptions.Filters = append(listOptions.Filters, query.FilterOption{Column: []string{"created_by"}, Values: []string{curUser.Username}})
		}
	}

	result, err := al.jobProvider.GetMultiJobSummaries(req.Context(), listOptions)
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, "Failed to get multi-client jobs.", err)
		return
	}

	totalCount, err := al.jobProvider.CountMultiJobs(req.Context(), listOptions)
	if err != nil {
		al.jsonErrorResponseWithError(w, http.StatusInternalServerError, "Failed to count multi-client jobs.", err)
		return
	}

	payload := &api.SuccessPayload{
		Data: result,
		Meta: api.NewMeta(totalCount),
	}
	al.writeJSONResponse(w, http.StatusOK, payload)
}
