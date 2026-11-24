// Package uploads is the file upload API: POST /files sends a file to clients (through the client
// dispatcher) and GET /ws/uploads streams each client's result.
package uploads

import (
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pkg/errors"

	"github.com/openrport/openrport/server/api"
	errors2 "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/auditlog"
	"github.com/openrport/openrport/server/cgroups"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/server/clients/targeting"
	"github.com/openrport/openrport/server/dispatch"
	"github.com/openrport/openrport/share/files"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/random"
)

// GroupLister lists the client groups of the tenant in ctx. cgroups.ClientGroupProvider satisfies it.
type GroupLister interface {
	GetAll(ctx context.Context) ([]*cgroups.ClientGroup, error)
}

// Options are the handler's dependencies; all are required.
type Options struct {
	// Config gives the upload directory.
	Config     *chconfig.Config
	Files      files.FileAPI
	Dispatcher dispatch.Dispatcher
	AuditLog   *auditlog.AuditLog
	Targets    targeting.Resolver
	Groups     GroupLister
	// CurrentUser returns the user of the request (an errors.APIError when there is none).
	CurrentUser func(ctx context.Context) (*users.User, error)
	Respond     *api.Responder
	Upgrader    *websocket.Upgrader
	Logger      *logger.Logger
}

// Handler serves the upload routes.
type Handler struct {
	config      *chconfig.Config
	files       files.FileAPI
	dispatcher  dispatch.Dispatcher
	auditLog    *auditlog.AuditLog
	targets     targeting.Resolver
	groups      GroupLister
	currentUser func(ctx context.Context) (*users.User, error)
	respond     *api.Responder
	upgrader    *websocket.Upgrader
	log         *logger.Logger
	// websockets are the open GET /ws/uploads connections, by connection ID.
	websockets sync.Map
}

// New returns the upload handler.
func New(o Options) *Handler {
	return &Handler{
		config:      o.Config,
		files:       o.Files,
		dispatcher:  o.Dispatcher,
		auditLog:    o.AuditLog,
		targets:     o.Targets,
		groups:      o.Groups,
		currentUser: o.CurrentUser,
		respond:     o.Respond,
		upgrader:    o.Upgrader,
		log:         o.Logger,
	}
}

// Close closes the open upload websockets.
func (h *Handler) Close() error {
	h.websockets.Range(func(_, value interface{}) bool {
		if wsConn, ok := value.(*websocket.Conn); ok {
			_ = wsConn.Close()
		}
		return true
	})
	return nil
}

const uploadBufSize = 1000000 // 1Mb

type UploadRequest struct {
	File                 multipart.File
	FileHeader           *multipart.FileHeader
	ClientIDs            []string
	GroupIDs             []string
	ClientTags           *models.JobClientTags
	clientsInGroupsCount int
	Clients              []*clientdata.Client
	*models.UploadedFile
}

func (ur UploadRequest) GetClientIDs() (ids []string) {
	return ur.ClientIDs
}

func (ur UploadRequest) GetGroupIDs() (ids []string) {
	return ur.GroupIDs
}

func (ur UploadRequest) GetClientTags() (clientTags *models.JobClientTags) {
	return ur.ClientTags
}

// HandleFileUploads handles POST /files: it stores the file and sends it to the targeted clients
// in the background.
func (h *Handler) HandleFileUploads(w http.ResponseWriter, req *http.Request) {
	uploadRequest, err := h.uploadRequestFromRequest(req)
	if err != nil {
		h.respond.Error(w, err)
		return
	}

	curUser, err := h.currentUser(req.Context())
	if err != nil {
		h.respond.ErrorWithTitle(w, http.StatusBadRequest, err.Error())
		return
	}
	defer uploadRequest.File.Close()

	wasCreated, err := h.files.CreateDirIfNotExists(h.config.GetUploadDir(), files.DefaultMode)
	if err != nil {
		h.respond.Error(w, err)
		return
	}
	if wasCreated {
		h.log.Infof("created directory %s", h.config.GetUploadDir())
	}

	uploadRequest.SourceFilePath = h.genFilePath(uploadRequest.ID)

	err = uploadRequest.Validate()
	if err != nil {
		h.respond.ErrorWithTitle(w, http.StatusBadRequest, err.Error())
		return
	}

	if err = validateRemoteDestination(uploadRequest); err != nil {
		h.respond.ErrorWithDetail(w, http.StatusBadRequest, "BAD_DESTINATION", "upload denied", err.Error())
		return
	}

	cr := clients.ClientServiceProvider{}
	clientGroups, err := h.groups.GetAll(req.Context())
	if err != nil {
		h.respond.Error(w, err)
		return
	}
	if err := cr.CheckClientsAccess(uploadRequest.Clients, curUser, clientGroups); err != nil {
		h.respond.ErrorWithDetail(w, http.StatusForbidden, "ACCESS_CONTROL_VIOLATION", "upload forbidden", err.Error())
		return
	}

	copiedBytes, err := h.files.CreateFile(uploadRequest.SourceFilePath, uploadRequest.File)
	if err != nil {
		h.respond.Error(w, err)
		return
	}

	file, err := h.files.Open(uploadRequest.SourceFilePath)
	if err != nil {
		h.respond.Error(w, err)
		return
	}

	md5Checksum, err := files.Md5HashFromReader(file)
	if err != nil {
		h.respond.Error(w, err)
		return
	}

	uploadRequest.Md5Checksum = md5Checksum

	h.log.Debugf(
		"stored file %s on server, size %d, Content-Type %s, temp location: %s, md5 checksum: %x",
		uploadRequest.FileHeader.Filename,
		uploadRequest.FileHeader.Size,
		uploadRequest.FileHeader.Header.Get("Content-Type"),
		uploadRequest.SourceFilePath,
		md5Checksum,
	)

	uploadRep := &models.UploadResponseShort{
		ID:        uploadRequest.ID,
		Filepath:  uploadRequest.DestinationPath,
		SizeBytes: copiedBytes,
	}
	h.auditLog.Entry(auditlog.ApplicationUploads, auditlog.ActionCreate).
		WithHTTPRequest(req).
		WithRequest(uploadRequest.UploadedFile).
		WithResponse(uploadRep).
		WithID(uploadRequest.UploadedFile.ID).
		SaveForMultipleClients(uploadRequest.Clients)

	// the sends outlive the request; they keep its tenant
	go h.sendFileToClients(context.WithoutCancel(req.Context()), uploadRequest)

	response := api.NewSuccessPayload(uploadRep)

	h.respond.JSON(w, http.StatusOK, response)
}

// HandleUploadsWS handles GET /ws/uploads: the websocket gets the result of every client upload.
func (h *Handler) HandleUploadsWS(w http.ResponseWriter, req *http.Request) {
	uiConn, err := h.upgrader.Upgrade(w, req, nil)
	if err != nil {
		h.log.Errorf("Failed to establish WS connection: %v", err)
		return
	}

	connID, err := uuid.NewUUID()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.websockets.Store(connID, uiConn)

	defer h.websockets.Delete(connID)
	defer uiConn.Close()

	for {
		_, _, err := uiConn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				h.log.Infof("closed ws connection: %v", err)
			}
			break
		}
	}
}

func (h *Handler) genFilePath(uuid string) string {
	uniqueFilename := fmt.Sprintf("%s_rport_filepush", uuid)

	return filepath.Join(h.config.GetUploadDir(), uniqueFilename)
}

type uploadResult struct {
	resp   *models.UploadResponse
	err    error
	client *clientdata.Client
}

type UploadOutput struct {
	ClientID string `json:"client_id"`
	*models.UploadResponse
}

func (h *Handler) sendFileToClients(ctx context.Context, uploadRequest *UploadRequest) {
	wg := &sync.WaitGroup{}
	wg.Add(len(uploadRequest.Clients))

	resChan := make(chan *uploadResult, len(uploadRequest.Clients))

	for _, cl := range uploadRequest.Clients {
		go h.sendFileToClient(ctx, wg, uploadRequest.UploadedFile, cl, resChan)
	}

	go func() {
		wg.Wait()
		close(resChan)
	}()

	h.consumeUploadResults(resChan, uploadRequest)

	err := h.files.Remove(uploadRequest.SourceFilePath)
	if err != nil {
		h.log.Errorf("failed to delete temp file path %s: %v", uploadRequest.SourceFilePath, err)
	}
}

func (h *Handler) consumeUploadResults(resChan chan *uploadResult, uploadRequest *UploadRequest) {
	for res := range resChan {
		clientID := res.client.GetID()
		output := &UploadOutput{
			ClientID:       clientID,
			UploadResponse: res.resp,
		}
		if res.err != nil {
			errTxt := res.err.Error()
			if errTxt == "client error: unknown request" {
				errTxt = "client doens't support uploads, please upgrade client to the latest version to make it work"
			}
			output.UploadResponse = &models.UploadResponse{
				Message: errTxt,
				Status:  "error",
				UploadResponseShort: models.UploadResponseShort{
					ID: uploadRequest.ID,
				},
			}
			h.log.Errorf(
				"upload failure: %s, file id: %s, file path: %s, client %s",
				errTxt,
				uploadRequest.ID,
				uploadRequest.DestinationPath,
				clientID,
			)
			h.auditLog.Entry(auditlog.ApplicationUploads, auditlog.ActionFailed).
				WithRequest(uploadRequest.UploadedFile).
				WithResponse(output).
				WithID(uploadRequest.UploadedFile.ID).
				WithClient(res.client).
				WithContext(res.client.GetContext()).
				Save()
		} else {
			h.log.Infof(
				"upload success, file id: %s, file path: %s, client %s",
				uploadRequest.ID,
				uploadRequest.DestinationPath,
				clientID,
			)
			h.auditLog.Entry(auditlog.ApplicationUploads, auditlog.ActionSuccess).
				WithRequest(uploadRequest.UploadedFile).
				WithResponse(output).
				WithID(uploadRequest.UploadedFile.ID).
				WithClient(res.client).
				WithContext(res.client.GetContext()).
				Save()
		}

		h.notifyUploadEventListeners(output)
	}
}

func (h *Handler) sendFileToClient(ctx context.Context, wg *sync.WaitGroup, file *models.UploadedFile, cl *clientdata.Client, resChan chan *uploadResult) {
	defer wg.Done()

	resp, err := h.dispatcher.Upload(ctx, cl.GetID(), file)
	resChan <- &uploadResult{
		err:    err,
		client: cl,
		resp:   resp,
	}
}

func (h *Handler) notifyUploadEventListeners(msg interface{}) {
	h.websockets.Range(func(key, value interface{}) bool {
		if wsConn, ok := value.(*websocket.Conn); ok {
			err := wsConn.WriteJSON(msg)
			if err != nil {
				h.log.Errorf("failed to send notification to websocket client %s: %v", key, err)
			}
		}
		return true
	})
}

func (h *Handler) uploadRequestFromRequest(req *http.Request) (ur *UploadRequest, err error) {
	ur = &UploadRequest{
		UploadedFile: &models.UploadedFile{},
	}

	err = req.ParseMultipartForm(uploadBufSize)
	if err != nil {
		return nil, &errors2.APIError{
			Err:        err,
			HTTPStatus: http.StatusBadRequest,
		}
	}

	ur.ClientIDs = req.MultipartForm.Value["client_id"]
	ur.GroupIDs = req.MultipartForm.Value["group_id"]

	clientTags, err := getClientTagsFromReqForm(req)
	if err != nil {
		return nil, &errors2.APIError{
			Err:        err,
			HTTPStatus: http.StatusBadRequest,
		}
	}

	ur.ClientTags = clientTags

	orderedClients, clientsInGroupsCount, err := h.targets.Resolve(req.Context(), ur)
	if err != nil {
		return nil, err
	}

	ur.Clients = orderedClients
	ur.clientsInGroupsCount = clientsInGroupsCount

	err = ur.FromMultipartRequest(req)
	if err != nil {
		return nil, &errors2.APIError{
			Err:        err,
			HTTPStatus: http.StatusBadRequest,
		}
	}

	ur.File, ur.FileHeader, err = req.FormFile("upload")
	if err != nil {
		return nil, &errors2.APIError{
			Err:        err,
			HTTPStatus: http.StatusBadRequest,
		}
	}

	if ur.UploadedFile.ID == "" {
		id, e := random.UUID4()
		if e != nil {
			h.log.Errorf("failed to generate uuid, will fallback to timestamp uuid, error: %v", e)
			id = fmt.Sprintf("%d", time.Now().UnixNano())
		}
		ur.UploadedFile.ID = id
	}

	return ur, nil
}

func getClientTagsFromReqForm(req *http.Request) (clientTags *models.JobClientTags, err error) {
	jsonTags := req.MultipartForm.Value["tags"]

	if len(jsonTags) == 0 {
		return nil, nil
	}

	if len(jsonTags) > 1 {
		return nil, errors.New("tags form value must only contain a single element")
	}

	clientTags = &models.JobClientTags{}
	err = json.Unmarshal([]byte(jsonTags[0]), clientTags)
	if err != nil {
		return nil, err
	}

	return clientTags, nil
}

func validateRemoteDestination(ur *UploadRequest) error {
	// deny uploads to the below unix folders because there is no reason why a user should do that.
	denied := []string{"/proc/", "/sys/", "/dev/", "/run/"}
	for _, v := range denied {
		if strings.HasPrefix(ur.DestinationPath, v) {
			return fmt.Errorf("uploads to %s are forbidden", v)
		}
	}
	return nil
}
