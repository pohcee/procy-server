package jobrunner

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	errors2 "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/server/clients/targeting"
	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/share/comm"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/ws"
)

func (r *Runner) RunJob(
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
		var resp *comm.RunCmdResponse
		if resp, err = r.dispatcher.RunJob(ctx, client.GetID(), &curJob); err == nil {
			sshResp = resp
		}
	} else {
		err = fmt.Errorf("client is paused (reason = %s)", client.PausedReason)
	}

	if err != nil {
		r.log.Errorf("%s, Error on execute remote command: %v", logPrefix, err)

		curJob.Status = models.JobStatusFailed
		now := time.Now()
		curJob.FinishedAt = &now
		curJob.Error = err.Error()

		// send the failed job to UI
		if uiConnTS != nil {
			_ = uiConnTS.WriteJSON(curJob)
		}
	} else {
		r.log.Debugf("%s, Job was sent to execute remote command: %q.", logPrefix, curJob.Command)

		// success, set fields received in response
		curJob.PID = &sshResp.Pid
		curJob.StartedAt = sshResp.StartedAt // override with the start time of the command
		curJob.Status = models.JobStatusRunning
	}

	// do not save the failed job if it's a single-client job
	if err != nil && multiJobID == nil {
		return err
	}

	if dbErr := r.jobs.CreateJob(ctx, &curJob); dbErr != nil {
		// just log it, cmd is running, when it's finished it can be saved on result return
		r.log.Errorf("%s, Failed to persist job: %v", logPrefix, dbErr)
	}

	return err
}

func (r *Runner) StartMultiClientJob(ctx context.Context, multiJobRequest *jobs.MultiJobRequest) (*models.MultiJob, error) {
	jid, err := r.newJobID()
	if err != nil {
		return nil, err
	}

	// by default abortOnErr is true
	abortOnErr := true
	if multiJobRequest.AbortOnError != nil {
		abortOnErr = *multiJobRequest.AbortOnError
	}
	if multiJobRequest.TimeoutSec <= 0 {
		multiJobRequest.TimeoutSec = r.config.Server.RunRemoteCmdTimeoutSec
	}

	if multiJobRequest.OrderedClients == nil {
		// try to rebuild the ordered client list
		if !targeting.HasClientTags(multiJobRequest) {
			multiJobRequest.OrderedClients, _, err = r.targets.Ordered(ctx, multiJobRequest.ClientIDs, multiJobRequest.GroupIDs)
			if err != nil {
				return nil, err
			}
		} else {
			multiJobRequest.OrderedClients, err = r.targets.ByTag(ctx, multiJobRequest.ClientTags)
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
	if err := r.jobs.SaveMultiJob(ctx, multiJob); err != nil {
		return nil, err
	}

	go r.executeMultiClientJob(ctx, multiJob, multiJobRequest.OrderedClients)

	return multiJob, nil
}

func (r *Runner) executeMultiClientJob(
	ctx context.Context,
	job *models.MultiJob,
	orderedClients []*clientdata.Client,
) {
	r.RunMultiClientJob(ctx, nil, job, orderedClients, SameSpecForAll(TypedJobSpec(job)))
}

// serverURLPlaceholder is filled with the public server URL in built-in entries.
const serverURLPlaceholder = "{{server_url}}"

// LibraryServerURL is the public server URL filled into built-in entries: empty when pairing is
// disabled (devices would get a 404 from /pairing/update) or no URL is configured.
func LibraryServerURL(cfg *chconfig.Config) string {
	if cfg == nil || !cfg.Pairing.Enabled {
		return ""
	}
	return strings.TrimRight(cfg.PairingPublicURL(), "/")
}

// StartLibraryMultiJob starts a run of a library entry. The entry's active
// version is resolved once, here, and every client runs it; a client whose OS
// has no implementation gets a skipped job. The username is taken from the
// request, like StartMultiClientJob.
func (r *Runner) StartLibraryMultiJob(ctx context.Context, entryID string, req *jobs.MultiJobRequest) (*models.MultiJob, error) {
	multiJob, specFor, err := r.PrepareLibraryMultiJob(ctx, entryID, req)
	if err != nil {
		return nil, err
	}
	go r.RunMultiClientJob(ctx, nil, multiJob, req.OrderedClients, specFor)
	return multiJob, nil
}

// PrepareLibraryMultiJob resolves the entry's active version, saves the
// multi-job and returns it with the per-client spec resolver. The caller runs it.
func (r *Runner) PrepareLibraryMultiJob(ctx context.Context, entryID string, req *jobs.MultiJobRequest) (*models.MultiJob, ClientSpecFunc, error) {
	entry, err := r.library.ActiveForRun(ctx, entryID)
	if err != nil {
		return nil, nil, err
	}

	jid, err := r.newJobID()
	if err != nil {
		return nil, nil, err
	}

	// by default abortOnErr is true
	abortOnErr := true
	if req.AbortOnError != nil {
		abortOnErr = *req.AbortOnError
	}

	if req.OrderedClients == nil {
		if !targeting.HasClientTags(req) {
			req.OrderedClients, _, err = r.targets.Ordered(ctx, req.ClientIDs, req.GroupIDs)
		} else {
			req.OrderedClients, err = r.targets.ByTag(ctx, req.ClientTags)
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
	serverURL := LibraryServerURL(r.config)
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
		timeoutSec = r.config.Server.RunRemoteCmdTimeoutSec
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
	if err := r.jobs.SaveMultiJob(ctx, multiJob); err != nil {
		return nil, nil, err
	}

	defaultTimeout := r.config.Server.RunRemoteCmdTimeoutSec
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

// ClientSpecFunc returns what to run on a client. A non-empty skipReason means
// the client is not sent anything and gets a skipped job instead.
type ClientSpecFunc func(c *clientdata.Client) (spec library.JobSpec, skipReason string)

// SameSpecForAll is the ClientSpecFunc of a typed run: every client runs spec.
func SameSpecForAll(spec library.JobSpec) ClientSpecFunc {
	return func(*clientdata.Client) (library.JobSpec, string) {
		return spec, ""
	}
}

// TypedJobSpec is the spec of a typed (non-library) multi-job.
func TypedJobSpec(job *models.MultiJob) library.JobSpec {
	return library.JobSpec{
		Command:     job.Command,
		Interpreter: job.Interpreter,
		Cwd:         job.Cwd,
		TimeoutSec:  job.TimeoutSec,
		IsSudo:      job.IsSudo,
		IsScript:    job.IsScript,
	}
}

// RunMultiClientJob runs job on clients, concurrently or one after another.
// In a sequential run it waits for each job to finish and, with AbortOnErr,
// stops at the first failure; a client not connected never aborts. A skipped
// client gets a saved job with status skipped and never waits or aborts.
//
// With uiConn set, failed and skipped jobs are written to it, an abort closes
// it, a job ID failure is reported on it, and after the last client it waits
// for the UI to close it (keeping the sequential done channel registered
// until then, as before).
//
// ctx carries the tenant the jobs are saved in. The run outlives the request that started it, so
// ctx's cancellation is dropped.
func (r *Runner) RunMultiClientJob(
	ctx context.Context,
	uiConn *ws.ConcurrentWebSocket,
	job *models.MultiJob,
	clients []*clientdata.Client,
	specFor ClientSpecFunc,
) {
	ctx = context.WithoutCancel(ctx)

	// for sequential execution - create a channel to get the job result
	var curJobDoneChannel chan *models.Job
	if !job.Concurrent {
		curJobDoneChannel = make(chan *models.Job)
		r.done.Set(job.JID, curJobDoneChannel)
		defer func() {
			close(curJobDoneChannel)
			r.done.Del(job.JID)
		}()
	}

	for _, client := range clients {
		curJID, err := r.newJobID()
		if err != nil {
			if uiConn != nil {
				uiConn.WriteError("Could not generate job id.", err)
				_ = uiConn.Close()
				AwaitUIClose(uiConn, r.log)
			}
			return
		}

		spec, skipReason := specFor(client)
		if skipReason != "" {
			r.saveSkippedJob(ctx, uiConn, &job.JID, curJID, job.CreatedBy, spec, skipReason, client)
			continue
		}

		if job.Concurrent {
			go r.RunJob(ctx, uiConn, &job.JID, curJID, job.CreatedBy, spec, client) //nolint:errcheck // error is logged, nothing to act on here
			continue
		}

		err = r.RunJob(ctx, uiConn, &job.JID, curJID, job.CreatedBy, spec, client)
		if err != nil {
			if job.AbortOnErr && !errors.Is(err, ErrClientNotConnected) {
				if uiConn != nil {
					// the failed job was written; end the stream with the closing handshake
					_ = uiConn.Close()
					AwaitUIClose(uiConn, r.log)
					return
				}
				break
			}
			continue
		}

		// TODO: review use of this flag as a testing hack. works but not too nice.
		// in tests skip next part to avoid waiting
		if r.noSequentialWait {
			continue
		}

		// wait until command is finished
		jobResult := <-curJobDoneChannel
		if job.AbortOnErr && jobResult.Status == models.JobStatusFailed {
			if uiConn != nil {
				_ = uiConn.Close()
				AwaitUIClose(uiConn, r.log)
				return
			}
			break
		}
	}

	if r.onRunDone != nil {
		r.onRunDone()
	}
	if uiConn != nil {
		AwaitUIClose(uiConn, r.log)
	}
}

// saveSkippedJob records a job that is not sent to its client and, with
// uiConn set, writes it there like a finished job.
func (r *Runner) saveSkippedJob(
	ctx context.Context,
	uiConn *ws.ConcurrentWebSocket,
	multiJobID *string,
	jid, createdBy string,
	spec library.JobSpec,
	reason string,
	client *clientdata.Client,
) {
	now := time.Now()
	skipped := newJob(uiConn, multiJobID, jid, createdBy, spec, client)
	skipped.StartedAt = now
	skipped.FinishedAt = &now
	skipped.Status = models.JobStatusSkipped
	skipped.Result = &models.JobResult{Summary: reason}

	r.log.Debugf("%s, Job skipped: %s", skipped.LogPrefix(), reason)

	if uiConn != nil {
		_ = uiConn.WriteJSON(skipped)
	}
	if err := r.jobs.CreateJob(ctx, &skipped); err != nil {
		r.log.Errorf("%s, Failed to persist skipped job: %v", skipped.LogPrefix(), err)
	}
}

func newJob(
	uiConn *ws.ConcurrentWebSocket,
	multiJobID *string,
	jid, createdBy string,
	spec library.JobSpec,
	client *clientdata.Client,
) models.Job {
	return models.Job{
		JID:            jid,
		StartedAt:      time.Now(),
		ClientID:       client.GetID(),
		ClientName:     client.GetName(),
		Command:        spec.Command,
		Cwd:            spec.Cwd,
		IsSudo:         spec.IsSudo,
		IsScript:       spec.IsScript,
		Interpreter:    spec.Interpreter,
		CreatedBy:      createdBy,
		TimeoutSec:     spec.TimeoutSec,
		MultiJobID:     multiJobID,
		StreamResult:   uiConn != nil,
		LibraryEntryID: spec.LibraryEntryID,
		LibraryVersion: spec.LibraryVersion,
		OS:             spec.OS,
	}
}

// AwaitUIClose blocks until the UI closes the websocket (or sends a message, which closes it from here), then
// closes the connection. Reading the UI's answer to the close frame lets the stream end with the closing handshake
// (1000) instead of a dropped connection.
func AwaitUIClose(uiConnTS *ws.ConcurrentWebSocket, log *logger.Logger) {
	defer uiConnTS.CloseNow()
	for {
		mt, message, err := uiConnTS.ReadMessage()
		if err != nil {
			if closeErr, ok := err.(*websocket.CloseError); ok {
				log.Debugf("Received a closed err on WS read: %v", closeErr)
				return
			}
			log.Debugf("Error read from websocket: %v", err)
			return
		}
		log.Debugf("Message received: type %v, msg %s", mt, message)
		// the UI is done: close (the close frame), then read its answer
		_ = uiConnTS.Close()
	}
}
