package operator

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/gorilla/mux"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/metering"
	"github.com/openrport/openrport/share/tenant"
)

// now is time.Now, replaced in tests.
var now = time.Now

// organizationUsage answers GET /operator/organizations/{id}/usage?from&to&granularity: the organization's usage
// report, as its administrators see it at GET /usage.
func (h *Handler) organizationUsage(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if _, err := h.svc.Get(r.Context(), id); err != nil {
		h.fail(w, err)
		return
	}
	qv := r.URL.Query()
	q, err := metering.ParseQuery(qv.Get("from"), qv.Get("to"), qv.Get("granularity"), now())
	if err != nil {
		h.fail(w, err)
		return
	}
	d, err := h.usage.Usage(r.Context(), tenant.ID(id), q)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(metering.BuildReport(q, d, now())))
}

// OrganizationUsage is an organization's line of the usage summary.
type OrganizationUsage struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Status string          `json:"status"`
	Totals metering.Totals `json:"totals"`
}

// UsageSummary is the usage of every organization over a range.
type UsageSummary struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Sort is the metric the organizations are ordered by, highest first.
	Sort          string              `json:"sort"`
	Organizations []OrganizationUsage `json:"organizations"`
}

// sortKeys are the totals the summary can be sorted by.
var sortKeys = map[string]func(t metering.Totals) float64{
	"transfer_bytes":          func(t metering.Totals) float64 { return float64(t.TransferBytes) },
	"tunnel_bytes_to_user":    func(t metering.Totals) float64 { return float64(t.TunnelBytesToUser) },
	"tunnel_bytes_to_device":  func(t metering.Totals) float64 { return float64(t.TunnelBytesToDevice) },
	"upload_bytes_to_devices": func(t metering.Totals) float64 { return float64(t.UploadBytesToDevices) },
	"api_bytes_out":           func(t metering.Totals) float64 { return float64(t.APIBytesOut) },
	"jobs_run":                func(t metering.Totals) float64 { return float64(t.JobsRun) },
	"device_days":             func(t metering.Totals) float64 { return float64(t.DeviceDays) },
	"devices_billable":        func(t metering.Totals) float64 { return float64(t.DevicesBillable) },
	"devices_peak":            func(t metering.Totals) float64 { return float64(t.DevicesPeak) },
	"device_hours":            func(t metering.Totals) float64 { return t.DeviceHours },
	"monitoring_rows":         func(t metering.Totals) float64 { return float64(t.MonitoringRows) },
}

// usageSummary answers GET /operator/usage?month= or ?from&to, with &organization= to list one and &metric= to sort
// by another total than transfer_bytes: every organization's totals, those without usage included.
func (h *Handler) usageSummary(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	var from, to time.Time
	var err error
	if qv.Get("from") != "" || qv.Get("to") != "" {
		if qv.Get("month") != "" {
			h.fail(w, fmt.Errorf("%w: give month or from and to, not both", ErrInvalid))
			return
		}
		var q metering.Query
		q, err = metering.ParseQuery(qv.Get("from"), qv.Get("to"), string(metering.Day), now())
		from, to = q.From, q.To
	} else {
		from, to, err = metering.ParseMonth(qv.Get("month"), now())
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	sortBy := qv.Get("metric")
	if sortBy == "" {
		sortBy = "transfer_bytes"
	}
	key, ok := sortKeys[sortBy]
	if !ok {
		h.fail(w, fmt.Errorf("%w: unknown metric %q", ErrInvalid, sortBy))
		return
	}
	orgs, err := h.svc.List(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	only := qv.Get("organization")
	rows, days, err := h.usage.Summary(r.Context(), from, to)
	if err != nil {
		h.fail(w, err)
		return
	}
	totals := metering.TenantTotals(rows, days, from, to, now())
	out := UsageSummary{From: from, To: to, Sort: sortBy, Organizations: []OrganizationUsage{}}
	for _, o := range orgs {
		if only != "" && o.ID != only {
			continue
		}
		out.Organizations = append(out.Organizations, OrganizationUsage{ID: o.ID, Name: o.Name, Status: o.Status, Totals: totals[o.ID]})
	}
	if only != "" && len(out.Organizations) == 0 {
		h.fail(w, ErrNotFound)
		return
	}
	sort.SliceStable(out.Organizations, func(i, j int) bool {
		return key(out.Organizations[i].Totals) > key(out.Organizations[j].Totals)
	})
	h.respond.JSON(w, http.StatusOK, api.NewSuccessPayload(out))
}
