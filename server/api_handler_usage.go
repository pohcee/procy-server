package chserver

import (
	"errors"
	"net/http"
	"time"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/metering"
	"github.com/openrport/openrport/share/tenant"
)

// handleGetUsage handles GET /usage?from&to&granularity: the usage report of the request's organization, for its
// administrators (usage metering on).
func (al *APIListener) handleGetUsage(w http.ResponseWriter, req *http.Request) {
	tid, err := tenant.FromContext(req.Context())
	if err != nil {
		al.jsonError(w, err)
		return
	}
	qv := req.URL.Query()
	now := time.Now()
	q, err := metering.ParseQuery(qv.Get("from"), qv.Get("to"), qv.Get("granularity"), now)
	if err != nil {
		if errors.Is(err, metering.ErrInvalid) {
			al.jsonErrorResponseWithTitle(w, http.StatusBadRequest, err.Error())
			return
		}
		al.jsonError(w, err)
		return
	}
	d, err := al.usage.Usage(req.Context(), tid, q)
	if err != nil {
		al.jsonError(w, err)
		return
	}
	al.writeJSONResponse(w, http.StatusOK, api.NewSuccessPayload(metering.BuildReport(q, d, now)))
}
