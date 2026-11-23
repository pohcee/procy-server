package chserver

import (
	"io"
	"net/http"

	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/auditlog"
	"github.com/openrport/openrport/share/ws"
)

// libraryRunWSRequest is the first message of /ws/library-entries: the usual
// targeting and execution options of /ws/commands plus the entry to run.
type libraryRunWSRequest struct {
	EntryID string `json:"entry_id"`
	jobs.MultiJobRequest
}

// handleLibraryEntriesWS handles GET /ws/library-entries
func (al *APIListener) handleLibraryEntriesWS(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	uiConn, err := apiUpgrader.Upgrade(w, req, nil)
	if err != nil {
		al.Errorf("Failed to establish WS connection: %v", err)
		return
	}
	uiConnTS := ws.NewConcurrentWebSocket(uiConn, al.Logger)

	inbound := &libraryRunWSRequest{}
	err = uiConnTS.ReadJSON(inbound)
	if err == io.EOF {
		uiConnTS.WriteError("Inbound message should contain non empty json object with entry data.", nil)
		return
	} else if err != nil {
		uiConnTS.WriteError("Invalid JSON data.", err)
		return
	}
	jobReq := &inbound.MultiJobRequest

	// unknown entry is reported before anything about the targets
	if _, err := al.libraryManager.ActiveForRun(ctx, inbound.EntryID); err != nil {
		uiConnTS.WriteError("", err)
		return
	}

	orderedClients, _, responseErr := al.getOrderedClientsWithValidation(ctx, jobReq)
	if responseErr != nil {
		uiConnTS.WriteError("", responseErr)
		return
	}
	jobReq.OrderedClients = orderedClients

	curUser, err := al.getUserModelForAuth(ctx)
	if err != nil {
		uiConnTS.WriteError("Could not get current user.", err)
		return
	}
	clientGroups, err := al.clientGroupProvider.GetAll(ctx)
	if err != nil {
		uiConnTS.WriteError("Could not get client groups", err)
		return
	}
	if err := al.clientService.CheckClientsAccess(orderedClients, curUser, clientGroups); err != nil {
		uiConnTS.WriteError(err.Error(), nil)
		return
	}
	jobReq.Username = curUser.Username

	// the active version is fixed here, once, for every device
	multiJob, specFor, err := al.prepareLibraryMultiJob(ctx, inbound.EntryID, jobReq)
	if err != nil {
		uiConnTS.WriteError("Failed to persist a new multi-client job.", err)
		return
	}

	al.Server.uiJobWebSockets.Set(multiJob.JID, uiConnTS)
	defer al.Server.uiJobWebSockets.Delete(multiJob.JID)

	al.auditLog.Entry(auditlog.ApplicationLibraryEntry, auditlog.ActionRun).
		WithHTTPRequest(req).
		WithRequest(inbound).
		WithID(multiJob.JID).
		SaveForMultipleClients(orderedClients)

	uiConnTS.SetWritesBeforeClose(len(orderedClients))

	al.runMultiClientJob(uiConnTS, multiJob, orderedClients, specFor)
}
