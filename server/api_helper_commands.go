package chserver

import (
	"context"
	"time"

	"github.com/pohcee/procy-server/server/api/jobs"
	"github.com/pohcee/procy-server/server/auditlog"
	"github.com/pohcee/procy-server/server/jobrunner"
	"github.com/pohcee/procy-server/server/library"
	"github.com/pohcee/procy-server/server/validation"
	"github.com/pohcee/procy-server/share/models"
	"github.com/pohcee/procy-server/share/ws"
)

func (al *APIListener) handleCommandsExecutionWS(
	ctx context.Context,
	uiConnTS *ws.ConcurrentWebSocket,
	inboundMsg *jobs.MultiJobRequest,
	auditLogEntry *auditlog.Entry,
) {
	if inboundMsg.Command == "" {
		uiConnTS.WriteError("Command cannot be empty.", nil)
		return
	}
	if err := validation.ValidateInterpreter(inboundMsg.Interpreter, inboundMsg.IsScript); err != nil {
		uiConnTS.WriteError("Invalid interpreter", err)
		return
	}

	if inboundMsg.TimeoutSec <= 0 {
		inboundMsg.TimeoutSec = al.config.Server.RunRemoteCmdTimeoutSec
	}

	curUser, err := al.getUserModelForAuth(ctx)
	if err != nil {
		uiConnTS.WriteError("Could not get current user.", err)
		return
	}

	err = al.extendedPermissionCommandRaw(ctx, inboundMsg.Command, curUser)
	if err != nil {
		uiConnTS.WriteError("Extended Permission failed with ", err)
		al.Debugf("extended \"commands\" permission middleware: %v", err.Error())
		return
	}

	clientGroups, err := al.clientGroupProvider.GetAll(ctx)
	if err != nil {
		uiConnTS.WriteError("Could not get client groups", err)
		return
	}
	err = al.clientService.CheckClientsAccess(inboundMsg.OrderedClients, curUser, clientGroups)
	if err != nil {
		uiConnTS.WriteError(err.Error(), nil)
		return
	}

	jid, err := generateNewJobID()
	if err != nil {
		uiConnTS.WriteError("Could not generate job id.", err)
		return
	}
	al.uiJobWebSockets.Set(jid, uiConnTS)
	defer al.uiJobWebSockets.Delete(jid)

	auditLogEntry.
		WithRequest(inboundMsg).
		WithID(jid).
		SaveForMultipleClients(inboundMsg.OrderedClients)

	createdBy := curUser.Username
	if len(inboundMsg.OrderedClients) > 0 {
		// by default abortOnErr is true
		abortOnErr := true
		if inboundMsg.AbortOnError != nil {
			abortOnErr = *inboundMsg.AbortOnError
		}

		multiJob := &models.MultiJob{
			MultiJobSummary: models.MultiJobSummary{
				JID:       jid,
				StartedAt: time.Now(),
				CreatedBy: createdBy,
			},
			ClientIDs:   inboundMsg.ClientIDs,
			GroupIDs:    inboundMsg.GroupIDs,
			ClientTags:  inboundMsg.ClientTags,
			Command:     inboundMsg.Command,
			Cwd:         inboundMsg.Cwd,
			Interpreter: inboundMsg.Interpreter,
			TimeoutSec:  inboundMsg.TimeoutSec,
			Concurrent:  inboundMsg.ExecuteConcurrently,
			AbortOnErr:  abortOnErr,
			IsSudo:      inboundMsg.IsSudo,
			IsScript:    inboundMsg.IsScript,
		}
		if err := al.jobProvider.SaveMultiJob(ctx, multiJob); err != nil {
			uiConnTS.WriteError("Failed to persist a new multi-client job.", err)
			return
		}

		al.Debugf("Multi-client Job[id=%q] created to execute remote command on clients %s, groups %s tags %s: %q.", multiJob.JID, inboundMsg.ClientIDs, inboundMsg.GroupIDs, inboundMsg.GetClientTags(), inboundMsg.Command)

		uiConnTS.SetWritesBeforeClose(len(inboundMsg.OrderedClients))

		al.jobRunner.RunMultiClientJob(ctx, uiConnTS, multiJob, inboundMsg.OrderedClients, jobrunner.SameSpecForAll(jobrunner.TypedJobSpec(multiJob)))
		return
	}

	// single client, no multi-job
	al.jobRunner.RunJob( //nolint:errcheck // error is logged, nothing to act on here
		ctx,
		uiConnTS,
		nil,
		jid,
		createdBy,
		library.JobSpec{
			Command:     inboundMsg.Command,
			Interpreter: inboundMsg.Interpreter,
			Cwd:         inboundMsg.Cwd,
			TimeoutSec:  inboundMsg.TimeoutSec,
			IsSudo:      inboundMsg.IsSudo,
			IsScript:    inboundMsg.IsScript,
		},
		inboundMsg.OrderedClients[0],
	)

	if al.testDone != nil {
		al.testDone <- true
	}

	jobrunner.AwaitUIClose(uiConnTS, al.Logger)
}
