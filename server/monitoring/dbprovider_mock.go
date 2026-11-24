package monitoring

import (
	"context"
	"time"

	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/query"
)

type DBProviderMock struct {
	GraphMetricsListPayload      []*ClientGraphMetricsPayload
	GraphMetricsGraphListPayload []*ClientGraphMetricsGraphPayload
	MetricsListPayload           []*ClientMetricsPayload
	ProcessesListPayload         []*ClientProcessesPayload
	MountpointsListPayload       []*ClientMountpointsPayload
}

func (p *DBProviderMock) CountByClientID(_ context.Context, _ string, _ *query.ListOptions) (int, error) {
	return 10, nil
}

func (p *DBProviderMock) ListProcessesByClientID(_ context.Context, _ string, _ *query.ListOptions) ([]*ClientProcessesPayload, error) {
	return p.ProcessesListPayload, nil
}

func (p *DBProviderMock) ListMountpointsByClientID(_ context.Context, _ string, _ *query.ListOptions) ([]*ClientMountpointsPayload, error) {
	return p.MountpointsListPayload, nil
}

func (p *DBProviderMock) ListGraphByClientID(context.Context, string, float64, *query.ListOptions, string) ([]*ClientGraphMetricsGraphPayload, error) {
	return p.GraphMetricsGraphListPayload, nil
}

func (p *DBProviderMock) ListMetricsByClientID(_ context.Context, _ string, _ *query.ListOptions) ([]*ClientMetricsPayload, error) {
	return p.MetricsListPayload, nil
}

func (p *DBProviderMock) ListGraphMetricsByClientID(_ context.Context, _ string, _ float64, _ *query.ListOptions) ([]*ClientGraphMetricsPayload, error) {
	return p.GraphMetricsListPayload, nil
}

func (p *DBProviderMock) CreateMeasurement(_ context.Context, _ *models.Measurement) error {
	return nil
}

func (p *DBProviderMock) DeleteMeasurementsBefore(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

func (p *DBProviderMock) Close() error {
	return nil
}
