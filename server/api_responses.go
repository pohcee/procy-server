package chserver

import (
	"net/http"

	"github.com/pohcee/procy-server/server/api"
)

// responder writes responses like every handler of the API.
func (al *APIListener) responder() *api.Responder {
	return &api.Responder{Log: al.Logger, ErrLog: al.errResponseLogger}
}

func (al *APIListener) writeJSONResponse(w http.ResponseWriter, statusCode int, response interface{}) {
	al.responder().JSON(w, statusCode, response)
}

func (al *APIListener) jsonErrorResponse(w http.ResponseWriter, statusCode int, err error) {
	al.responder().ErrorResponse(w, statusCode, err)
}

func (al *APIListener) jsonError(w http.ResponseWriter, err error) {
	al.responder().Error(w, err)
}

func (al *APIListener) jsonErrorResponseWithErrCode(w http.ResponseWriter, statusCode int, errCode, title string) {
	al.responder().ErrorWithErrCode(w, statusCode, errCode, title)
}

func (al *APIListener) jsonErrorResponseWithTitle(w http.ResponseWriter, statusCode int, title string) {
	al.responder().ErrorWithTitle(w, statusCode, title)
}

func (al *APIListener) jsonErrorResponseWithDetail(w http.ResponseWriter, statusCode int, errCode, title, detail string) {
	al.responder().ErrorWithDetail(w, statusCode, errCode, title, detail)
}

func (al *APIListener) jsonErrorResponseWithError(w http.ResponseWriter, statusCode int, title string, err error) {
	al.responder().ErrorWithError(w, statusCode, title, err)
}
