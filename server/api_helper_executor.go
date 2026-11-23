package chserver

import (
	"errors"
	"time"

	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/ws"
)

// clientSpecFunc returns what to run on a client. A non-empty skipReason means
// the client is not sent anything and gets a skipped job instead.
type clientSpecFunc func(c *clientdata.Client) (spec library.JobSpec, skipReason string)

// sameSpecForAll is the clientSpecFunc of a typed run: every client runs spec.
func sameSpecForAll(spec library.JobSpec) clientSpecFunc {
	return func(*clientdata.Client) (library.JobSpec, string) {
		return spec, ""
	}
}

// typedJobSpec is the spec of a typed (non-library) multi-job.
func typedJobSpec(job *models.MultiJob) library.JobSpec {
	return library.JobSpec{
		Command:     job.Command,
		Interpreter: job.Interpreter,
		Cwd:         job.Cwd,
		TimeoutSec:  job.TimeoutSec,
		IsSudo:      job.IsSudo,
		IsScript:    job.IsScript,
	}
}

// runMultiClientJob runs job on clients, concurrently or one after another.
// In a sequential run it waits for each job to finish and, with AbortOnErr,
// stops at the first failure; a client not connected never aborts. A skipped
// client gets a saved job with status skipped and never waits or aborts.
//
// With uiConn set, failed and skipped jobs are written to it, an abort closes
// it, a job ID failure is reported on it, and after the last client it waits
// for the UI to close it (keeping the sequential done channel registered
// until then, as before).
func (al *APIListener) runMultiClientJob(
	uiConn *ws.ConcurrentWebSocket,
	job *models.MultiJob,
	clients []*clientdata.Client,
	specFor clientSpecFunc,
) {
	// for sequential execution - create a channel to get the job result
	var curJobDoneChannel chan *models.Job
	if !job.Concurrent {
		curJobDoneChannel = make(chan *models.Job)
		al.jobsDoneChannel.Set(job.JID, curJobDoneChannel)
		defer func() {
			close(curJobDoneChannel)
			al.jobsDoneChannel.Del(job.JID)
		}()
	}

	for _, client := range clients {
		curJID, err := generateNewJobID()
		if err != nil {
			if uiConn != nil {
				uiConn.WriteError("Could not generate job id.", err)
			}
			return
		}

		spec, skipReason := specFor(client)
		if skipReason != "" {
			al.saveSkippedJob(uiConn, &job.JID, curJID, job.CreatedBy, spec, skipReason, client)
			continue
		}

		if job.Concurrent {
			go al.createAndRunJob(uiConn, &job.JID, curJID, job.CreatedBy, spec, client) //nolint:errcheck // error is logged, nothing to act on here
			continue
		}

		err = al.createAndRunJob(uiConn, &job.JID, curJID, job.CreatedBy, spec, client)
		if err != nil {
			if job.AbortOnErr && !errors.Is(err, ErrClientNotConnected) {
				if uiConn != nil {
					uiConn.Close()
					return
				}
				break
			}
			continue
		}

		// TODO: review use of this flag as a testing hack. works but not too nice.
		// in tests skip next part to avoid waiting
		if al.insecureForTests {
			continue
		}

		// wait until command is finished
		jobResult := <-curJobDoneChannel
		if job.AbortOnErr && jobResult.Status == models.JobStatusFailed {
			if uiConn != nil {
				uiConn.Close()
				return
			}
			break
		}
	}

	if al.testDone != nil {
		al.testDone <- true
	}
	if uiConn != nil {
		al.awaitUIClose(uiConn)
	}
}

// saveSkippedJob records a job that is not sent to its client and, with
// uiConn set, writes it there like a finished job.
func (al *APIListener) saveSkippedJob(
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

	al.Debugf("%s, Job skipped: %s", skipped.LogPrefix(), reason)

	if uiConn != nil {
		_ = uiConn.WriteJSON(skipped)
	}
	if err := al.jobProvider.CreateJob(&skipped); err != nil {
		al.Errorf("%s, Failed to persist skipped job: %v", skipped.LogPrefix(), err)
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
