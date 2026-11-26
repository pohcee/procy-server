package peer

import (
	"encoding/json"
	"errors"

	errors2 "github.com/pohcee/procy-server/server/api/errors"
	"github.com/pohcee/procy-server/server/dispatch"
	"github.com/pohcee/procy-server/share/comm"
	errors3 "github.com/pohcee/procy-server/share/errors"
	"github.com/pohcee/procy-server/share/tenant"
)

// The peer API: POST requests with JSON bodies to these paths on a device process. Answers are JSON; the run-job
// answer is a stream of newline-delimited JSON frames. Every request names the tenant and the client; the device
// process trusts an api process to name the tenant it serves.
const (
	pathCall   = "/peer/v1/call"
	pathRunJob = "/peer/v1/run-job"
	pathUpload = "/peer/v1/upload"

	// headerUpload carries the upload request (JSON) of a pathUpload call, whose body is the file.
	headerUpload = "X-Rport-Peer-Upload"
)

// Operations of a pathCall request.
const (
	opIsTunnelAllowed      = "is_tunnel_allowed"
	opCheckPort            = "check_port"
	opStartTunnels         = "start_tunnels"
	opCloseTunnel          = "close_tunnel"
	opSetTunnelACL         = "set_tunnel_acl"
	opRefreshUpdatesStatus = "refresh_updates_status"
	opUpdateAttributes     = "update_attributes"
)

// target names the client a peer call is for.
type target struct {
	Tenant   tenant.ID `json:"tenant"`
	ClientID string    `json:"client_id"`
}

type callRequest struct {
	target
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

type callResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireError      `json:"error,omitempty"`
}

type remoteArgs struct {
	Remote string `json:"remote"`
}

type tunnelArgs struct {
	TunnelID string  `json:"tunnel_id"`
	Force    bool    `json:"force,omitempty"`
	ACL      *string `json:"acl,omitempty"`
}

// Frames of a run-job answer, in order: one "started" (or "error"), then any number of "output" and at most one
// "result", which ends the stream.
const (
	frameStarted = "started"
	frameOutput  = "output"
	frameResult  = "result"
	frameError   = "error"
)

type frame struct {
	Type string `json:"type"`
	// Data is the started response (comm.RunCmdResponse), or the message the client listener wrote to the UI job
	// stream (output: a non-final message; result: the job).
	Data  json.RawMessage `json:"data,omitempty"`
	Error *wireError      `json:"error,omitempty"`
}

type runJobRequest struct {
	target
	Job json.RawMessage `json:"job"`
}

type uploadRequest struct {
	target
	File json.RawMessage `json:"file"`
}

type uploadResponse struct {
	Response json.RawMessage `json:"response,omitempty"`
	Error    *wireError      `json:"error,omitempty"`
}

// wireError carries an error of the local dispatcher across a peer call so that the api process answers the
// request exactly as when the client is connected to it.
type wireError struct {
	Kind       string `json:"kind"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
	ErrCode    string `json:"err_code,omitempty"`
}

const (
	kindNotConnected    = "not_connected"
	kindTunnelNotFound  = "tunnel_not_found"
	kindUploadsDisabled = "uploads_disabled"
	kindClientError     = "client_error"
	kindAPIError        = "api_error"
	kindOther           = "other"
)

func toWire(err error) *wireError {
	if err == nil {
		return nil
	}
	var ce *comm.ClientError
	var ae errors2.APIError
	switch {
	case errors.Is(err, dispatch.ErrClientNotConnected):
		return &wireError{Kind: kindNotConnected, Message: err.Error()}
	case errors.Is(err, dispatch.ErrTunnelNotFound):
		return &wireError{Kind: kindTunnelNotFound, Message: err.Error()}
	case errors.Is(err, errors3.ErrUploadsDisabled):
		return &wireError{Kind: kindUploadsDisabled, Message: err.Error()}
	case errors.As(err, &ce):
		return &wireError{Kind: kindClientError, Message: err.Error()}
	case errors.As(err, &ae):
		return &wireError{Kind: kindAPIError, Message: ae.Message, HTTPStatus: ae.HTTPStatus, ErrCode: ae.ErrCode}
	default:
		return &wireError{Kind: kindOther, Message: err.Error()}
	}
}

func (w *wireError) err() error {
	if w == nil {
		return nil
	}
	switch w.Kind {
	case kindNotConnected:
		return dispatch.ErrClientNotConnected
	case kindTunnelNotFound:
		return dispatch.ErrTunnelNotFound
	case kindUploadsDisabled:
		return errors3.ErrUploadsDisabled
	case kindClientError:
		return comm.NewClientError(errors.New(w.Message))
	case kindAPIError:
		return errors2.APIError{Message: w.Message, HTTPStatus: w.HTTPStatus, ErrCode: w.ErrCode}
	default:
		return errors.New(w.Message)
	}
}
