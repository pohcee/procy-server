package chserver

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	errors2 "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/share/comm"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/random"
	"github.com/openrport/openrport/share/ws"
)

var ErrClientNotConnected = errors.New("client is not connected")

var generateNewJobID = func() (string, error) {
	return random.UUID4()
}

type JobProvider interface {
	GetByJID(ctx context.Context, clientID, jid string) (*models.Job, error)
	List(ctx context.Context, options *query.ListOptions) ([]*models.Job, error)
	Count(ctx context.Context, options *query.ListOptions) (int, error)
	// SaveJob creates or updates a job
	SaveJob(ctx context.Context, job *models.Job) error
	// CreateJob creates a new job. If already exist with a given JID - do nothing and return nil
	CreateJob(ctx context.Context, job *models.Job) error
	GetMultiJob(ctx context.Context, jid string) (*models.MultiJob, error)
	GetMultiJobSummaries(ctx context.Context, options *query.ListOptions) ([]*models.MultiJobSummary, error)
	CountMultiJobs(ctx context.Context, options *query.ListOptions) (int, error)
	SaveMultiJob(ctx context.Context, multiJob *models.MultiJob) error
	CleanupJobsMultiJobs(context.Context, int) error
	Close() error
}

func (al *APIListener) createAndRunJob(
	ctx context.Context,
	uiConnTS *ws.ConcurrentWebSocket,
	multiJobID *string,
	jid, createdBy string,
	spec library.JobSpec,
	client *clientdata.Client,
) error {
	curJob := newJob(uiConnTS, multiJobID, jid, createdBy, spec, client)
	logPrefix := curJob.LogPrefix()

	// send the command to the client
	sshResp := &comm.RunCmdResponse{}

	var err error
	if !client.IsPaused() {
		if client.Connection != nil {
			err = comm.SendRequestAndGetResponse(client.GetConnection(), comm.RequestTypeRunCmd, curJob, sshResp, al.Log())
		} else {
			err = ErrClientNotConnected
		}
	} else {
		err = fmt.Errorf("client is paused (reason = %s)", client.PausedReason)
	}

	if err != nil {
		al.Errorf("%s, Error on execute remote command: %v", logPrefix, err)

		curJob.Status = models.JobStatusFailed
		now := time.Now()
		curJob.FinishedAt = &now
		curJob.Error = err.Error()

		// send the failed job to UI
		if uiConnTS != nil {
			_ = uiConnTS.WriteJSON(curJob)
		}
	} else {
		al.Debugf("%s, Job was sent to execute remote command: %q.", logPrefix, curJob.Command)

		// success, set fields received in response
		curJob.PID = &sshResp.Pid
		curJob.StartedAt = sshResp.StartedAt // override with the start time of the command
		curJob.Status = models.JobStatusRunning
	}

	// do not save the failed job if it's a single-client job
	if err != nil && multiJobID == nil {
		return err
	}

	if dbErr := al.jobProvider.CreateJob(ctx, &curJob); dbErr != nil {
		// just log it, cmd is running, when it's finished it can be saved on result return
		al.Errorf("%s, Failed to persist job: %v", logPrefix, dbErr)
	}

	return err
}

func (al *APIListener) StartMultiClientJob(ctx context.Context, multiJobRequest *jobs.MultiJobRequest) (*models.MultiJob, error) {
	jid, err := generateNewJobID()
	if err != nil {
		return nil, err
	}

	// by default abortOnErr is true
	abortOnErr := true
	if multiJobRequest.AbortOnError != nil {
		abortOnErr = *multiJobRequest.AbortOnError
	}
	if multiJobRequest.TimeoutSec <= 0 {
		multiJobRequest.TimeoutSec = al.config.Server.RunRemoteCmdTimeoutSec
	}

	if multiJobRequest.OrderedClients == nil {
		// try to rebuild the ordered client list
		if !hasClientTags(multiJobRequest) {
			multiJobRequest.OrderedClients, _, err = al.getOrderedClients(ctx, multiJobRequest.ClientIDs, multiJobRequest.GroupIDs)
			if err != nil {
				return nil, err
			}
		} else {
			multiJobRequest.OrderedClients, err = al.getOrderedClientsByTag(multiJobRequest.ClientTags)
			if err != nil {
				return nil, err
			}
		}
	}

	if len(multiJobRequest.OrderedClients) == 0 {
		return nil, fmt.Errorf("no clients for execution")
	}

	command := multiJobRequest.Command
	if multiJobRequest.IsScript {
		decodedScriptBytes, err := base64.StdEncoding.DecodeString(multiJobRequest.Script)
		if err != nil {
			return nil, err
		}
		command = string(decodedScriptBytes)
	}

	multiJob := &models.MultiJob{
		MultiJobSummary: models.MultiJobSummary{
			JID:        jid,
			StartedAt:  time.Now(),
			CreatedBy:  multiJobRequest.Username,
			ScheduleID: multiJobRequest.ScheduleID,
		},
		ClientIDs:   multiJobRequest.ClientIDs,
		GroupIDs:    multiJobRequest.GroupIDs,
		ClientTags:  multiJobRequest.ClientTags,
		Command:     command,
		Interpreter: multiJobRequest.Interpreter,
		Cwd:         multiJobRequest.Cwd,
		IsScript:    multiJobRequest.IsScript,
		IsSudo:      multiJobRequest.IsSudo,
		TimeoutSec:  multiJobRequest.TimeoutSec,
		Concurrent:  multiJobRequest.ExecuteConcurrently,
		AbortOnErr:  abortOnErr,
	}
	if err := al.jobProvider.SaveMultiJob(ctx, multiJob); err != nil {
		return nil, err
	}

	go al.executeMultiClientJob(ctx, multiJob, multiJobRequest.OrderedClients)

	return multiJob, nil
}

func (al *APIListener) executeMultiClientJob(
	ctx context.Context,
	job *models.MultiJob,
	orderedClients []*clientdata.Client,
) {
	al.runMultiClientJob(ctx, nil, job, orderedClients, sameSpecForAll(typedJobSpec(job)))
}

// serverURLPlaceholder is filled with the public server URL in built-in entries.
const serverURLPlaceholder = "{{server_url}}"

// libraryServerURL is the public server URL filled into built-in entries:
// empty when pairing is disabled (devices would get a 404 from /pairing/update)
// or no URL is configured.
func (al *APIListener) libraryServerURL() string {
	if al.Server == nil || al.config == nil || !al.config.Pairing.Enabled {
		return ""
	}
	return strings.TrimRight(al.config.PairingPublicURL(), "/")
}

// libraryServeVars are the placeholder values filled into built-in entries the
// API serves; nil when the server URL is unavailable (placeholder stays).
func (al *APIListener) libraryServeVars() map[string]string {
	if u := al.libraryServerURL(); u != "" {
		return map[string]string{"server_url": u}
	}
	return nil
}

// StartLibraryMultiJob starts a run of a library entry. The entry's active
// version is resolved once, here, and every client runs it; a client whose OS
// has no implementation gets a skipped job. The username is taken from the
// request, like StartMultiClientJob.
func (al *APIListener) StartLibraryMultiJob(ctx context.Context, entryID string, req *jobs.MultiJobRequest) (*models.MultiJob, error) {
	multiJob, specFor, err := al.prepareLibraryMultiJob(ctx, entryID, req)
	if err != nil {
		return nil, err
	}
	go al.runMultiClientJob(ctx, nil, multiJob, req.OrderedClients, specFor)
	return multiJob, nil
}

// prepareLibraryMultiJob resolves the entry's active version, saves the
// multi-job and returns it with the per-client spec resolver. The caller runs it.
func (al *APIListener) prepareLibraryMultiJob(ctx context.Context, entryID string, req *jobs.MultiJobRequest) (*models.MultiJob, clientSpecFunc, error) {
	entry, err := al.libraryManager.ActiveForRun(ctx, entryID)
	if err != nil {
		return nil, nil, err
	}

	jid, err := generateNewJobID()
	if err != nil {
		return nil, nil, err
	}

	// by default abortOnErr is true
	abortOnErr := true
	if req.AbortOnError != nil {
		abortOnErr = *req.AbortOnError
	}

	if req.OrderedClients == nil {
		if !hasClientTags(req) {
			req.OrderedClients, _, err = al.getOrderedClients(ctx, req.ClientIDs, req.GroupIDs)
		} else {
			req.OrderedClients, err = al.getOrderedClientsByTag(req.ClientTags)
		}
		if err != nil {
			return nil, nil, err
		}
	}
	if len(req.OrderedClients) == 0 {
		return nil, nil, fmt.Errorf("no clients for execution")
	}

	// a run where no target can execute the entry fails instead of recording
	// only skipped jobs
	counts := map[string]int{}
	resolved := 0
	serverURL := al.libraryServerURL()
	needsServerURL := false
	for _, c := range req.OrderedClients {
		osKernel := c.GetOSKernel()
		counts[strings.ToLower(osKernel)]++
		if spec, ok := library.SpecFor(*entry, osKernel, nil); ok {
			resolved++
			if entry.BuiltinKey != nil && strings.Contains(spec.Command, serverURLPlaceholder) {
				needsServerURL = true
			}
		}
	}
	if resolved == 0 {
		oses := make([]string, 0, len(counts))
		for k := range counts {
			oses = append(oses, k)
		}
		sort.Strings(oses)
		parts := make([]string, 0, len(oses))
		for _, k := range oses {
			parts = append(parts, fmt.Sprintf("%s: %d", k, counts[k]))
		}
		msg := fmt.Sprintf("entry %q has no implementation for any target (targets %s)", entry.Name, strings.Join(parts, ", "))
		return nil, nil, errors2.APIError{Message: msg, Err: errors.New(msg), HTTPStatus: http.StatusBadRequest}
	}

	if needsServerURL && serverURL == "" {
		msg := fmt.Sprintf("enable [pairing] and set its url (or api.base_url) to use the built-in %q entry", entry.Name)
		return nil, nil, errors2.APIError{Message: msg, Err: errors.New(msg), HTTPStatus: http.StatusBadRequest}
	}
	vars := map[string]string{"server_url": serverURL}

	timeoutSec := library.MaxTimeout(*entry)
	if timeoutSec <= 0 {
		timeoutSec = al.config.Server.RunRemoteCmdTimeoutSec
	}
	multiJob := &models.MultiJob{
		MultiJobSummary: models.MultiJobSummary{
			JID:        jid,
			StartedAt:  time.Now(),
			CreatedBy:  req.Username,
			ScheduleID: req.ScheduleID,
		},
		ClientIDs:      req.ClientIDs,
		GroupIDs:       req.GroupIDs,
		ClientTags:     req.ClientTags,
		TimeoutSec:     timeoutSec,
		Concurrent:     req.ExecuteConcurrently,
		AbortOnErr:     abortOnErr,
		LibraryEntryID: entry.ID,
		LibraryVersion: entry.ActiveVersion,
	}
	if err := al.jobProvider.SaveMultiJob(ctx, multiJob); err != nil {
		return nil, nil, err
	}

	defaultTimeout := al.config.Server.RunRemoteCmdTimeoutSec
	specFor := func(c *clientdata.Client) (library.JobSpec, string) {
		osKernel := c.GetOSKernel()
		spec, ok := library.SpecFor(*entry, osKernel, vars)
		if !ok {
			return library.JobSpec{
				LibraryEntryID: entry.ID,
				LibraryVersion: entry.ActiveVersion,
				OS:             osKernel,
			}, "no " + library.OSLabel(osKernel) + " implementation"
		}
		if spec.TimeoutSec <= 0 {
			spec.TimeoutSec = defaultTimeout
		}
		return spec, ""
	}

	return multiJob, specFor, nil
}
