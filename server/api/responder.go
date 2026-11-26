package api

import (
	"encoding/json"
	"errors"
	"net/http"

	errors2 "github.com/pohcee/procy-server/server/api/errors"
	"github.com/pohcee/procy-server/share/logger"
)

// Responder writes the API's JSON responses and error payloads. Handler packages use it so their
// responses are the same as the rest of the API.
type Responder struct {
	// Log gets the errors writing a response.
	Log *logger.Logger
	// ErrLog, when at debug level, gets every error payload.
	ErrLog *logger.Logger
}

// logErrorPayload logs a server error (5xx: its body alone doesn't say why, and nothing else logs it) and, at
// debug level, every error payload.
func (r *Responder) logErrorPayload(statusCode int, errPayload ErrorPayload) {
	if statusCode >= http.StatusInternalServerError && r.Log != nil {
		r.Log.Errorf("api error response %d: %+v", statusCode, errPayload.Errors)
	}
	if r.ErrLog != nil && r.ErrLog.Level == logger.LogLevelDebug {
		r.ErrLog.Debugf("payload: %+v", errPayload)
	}
}

func (r *Responder) JSON(w http.ResponseWriter, statusCode int, response interface{}) {
	b, err := json.Marshal(response)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(statusCode)
	if _, err := w.Write(b); err != nil {
		r.Log.Errorf("error writing response: %s", err)
	}
}

func (r *Responder) ErrorResponse(w http.ResponseWriter, statusCode int, err error) {
	errPayload := NewErrAPIPayloadFromError(err, "", "")
	r.logErrorPayload(statusCode, errPayload)
	r.JSON(w, statusCode, errPayload)
}

func (r *Responder) Error(w http.ResponseWriter, err error) {
	statusCode := http.StatusInternalServerError
	errCode := ""
	message := ""
	var apiErr errors2.APIError
	var apiErrs errors2.APIErrors
	switch {
	case errors.As(err, &apiErr):
		statusCode = apiErr.HTTPStatus
		errCode = apiErr.ErrCode
		message = apiErr.Message
	case errors.As(err, &apiErrs):
		if len(apiErrs) > 0 {
			statusCode = apiErrs[0].HTTPStatus
			errCode = apiErrs[0].ErrCode
			message = apiErrs[0].Message
		}
	}

	errPayload := NewErrAPIPayloadFromError(err, errCode, message)
	r.logErrorPayload(statusCode, errPayload)
	r.JSON(w, statusCode, errPayload)
}

func (r *Responder) ErrorWithErrCode(w http.ResponseWriter, statusCode int, errCode, title string) {
	errPayload := NewErrAPIPayloadFromMessage(errCode, title, "")
	r.logErrorPayload(statusCode, errPayload)
	r.JSON(w, statusCode, errPayload)
}

func (r *Responder) ErrorWithTitle(w http.ResponseWriter, statusCode int, title string) {
	errPayload := NewErrAPIPayloadFromMessage("", title, "")
	r.logErrorPayload(statusCode, errPayload)
	r.JSON(w, statusCode, errPayload)
}

func (r *Responder) ErrorWithDetail(w http.ResponseWriter, statusCode int, errCode, title, detail string) {
	errPayload := NewErrAPIPayloadFromMessage(errCode, title, detail)
	r.logErrorPayload(statusCode, errPayload)
	r.JSON(w, statusCode, errPayload)
}

func (r *Responder) ErrorWithError(w http.ResponseWriter, statusCode int, title string, err error) {
	var detail string
	if err != nil {
		detail = err.Error()
	}
	errPayload := NewErrAPIPayloadFromMessage("", title, detail)
	r.logErrorPayload(statusCode, errPayload)
	r.JSON(w, statusCode, errPayload)
}
