package chserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/metering"
	"github.com/openrport/openrport/share/tenant"
)

type fakeUsage struct{ asked []tenant.ID }

func (f *fakeUsage) Usage(_ context.Context, tid tenant.ID, q metering.Query) (metering.Data, error) {
	f.asked = append(f.asked, tid)
	return metering.Data{Periods: []metering.PeriodRow{{Period: q.From, Metric: string(metering.TunnelBytesToUser), Value: 42}}}, nil
}

func (f *fakeUsage) Summary(context.Context, time.Time, time.Time) ([]metering.TenantRow, []metering.TenantDays, error) {
	return nil, nil, nil
}

// A standalone server (no metering) has no usage route and reports no usage capability.
func TestNoUsageAPIWithoutMetering(t *testing.T) {
	al := &APIListener{
		insecureForTests: true,
		apiServices:      &apiServices{config: &chconfig.Config{API: chconfig.APIConfig{MaxRequestBytes: 1024 * 1024}}},
		Logger:           testLog,
	}
	al.initRouter()
	w := httptest.NewRecorder()
	al.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/usage", nil))
	assert.Equal(t, http.StatusNotFound, w.Code)

	data := getStatusData(t, newStatusTestListener(&chconfig.Config{}, nil))
	assert.NotContains(t, string(data["capabilities"]), "usage")
}

// GET /usage reports the request's tenant only, whatever the query asks.
func TestUsageReportsTheRequestTenant(t *testing.T) {
	f := &fakeUsage{}
	al := newStatusTestListener(&chconfig.Config{}, nil)
	al.usage = f
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/usage?tenant=b&organization=b", nil).WithContext(tenant.WithID(context.Background(), "a"))
	al.handleGetUsage(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []tenant.ID{"a"}, f.asked)
	var body struct {
		Data metering.Report `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.EqualValues(t, 42, body.Data.Totals.TunnelBytesToUser)

	data := getStatusData(t, al)
	assert.Contains(t, string(data["capabilities"]), `"usage":true`)
}

type recWriter struct{ recs []metering.Record }

func (w *recWriter) WriteUsage(_ context.Context, recs []metering.Record) error {
	w.recs = append(w.recs, recs...)
	return nil
}

// API responses are counted as api_bytes_out of the request's tenant; reserved tenants are not counted.
func TestMeterResponsesCountsBytesPerTenant(t *testing.T) {
	w := &recWriter{}
	m := metering.New(metering.Options{Node: "n", Writer: w})
	h := meterResponses(m)(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		_, _ = rw.Write(make([]byte, 1000))
		_, _ = rw.Write(make([]byte, 24))
	}))
	for _, tid := range []tenant.ID{"a", "a", "_system"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil).WithContext(tenant.WithID(context.Background(), tid)))
	}
	require.NoError(t, m.Flush(context.Background()))
	require.Len(t, w.recs, 1)
	assert.Equal(t, tenant.ID("a"), w.recs[0].Tenant)
	require.Len(t, w.recs[0].Deltas, 1)
	assert.Equal(t, metering.APIBytesOut, w.recs[0].Deltas[0].Metric)
	assert.EqualValues(t, 2048, w.recs[0].Deltas[0].Value)
}
