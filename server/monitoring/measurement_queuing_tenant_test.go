package monitoring_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/monitoring"
	"github.com/pohcee/procy-server/share/models"
	"github.com/pohcee/procy-server/share/tenant"
)

type savedCall struct {
	clientID  string
	tenant    tenant.ID
	tenantErr error
	ctxErr    error
}

type recordingSaver struct {
	mu    sync.Mutex
	calls []savedCall
	delay time.Duration
	saved chan struct{}
}

func (s *recordingSaver) SaveMeasurement(ctx context.Context, m *models.Measurement) error {
	time.Sleep(s.delay)
	id, err := tenant.FromContext(ctx)
	s.mu.Lock()
	s.calls = append(s.calls, savedCall{clientID: m.ClientID, tenant: id, tenantErr: err, ctxErr: ctx.Err()})
	s.mu.Unlock()
	s.saved <- struct{}{}
	return nil
}

func TestQueueSavesWithTheEnqueuersTenant(t *testing.T) {
	saver := &recordingSaver{saved: make(chan struct{}, 10)}
	// the base context carries no tenant: the tenant must travel with the queued item
	q := monitoring.NewMeasurementQueuing(context.Background(), testLog, saver, 10)
	defer q.Close()

	require.True(t, q.Notify(tenant.WithID(context.Background(), "a"), models.Measurement{ClientID: "c1"}))
	require.True(t, q.Notify(tenant.WithID(context.Background(), "b"), models.Measurement{ClientID: "c2"}))
	<-saver.saved
	<-saver.saved

	saver.mu.Lock()
	defer saver.mu.Unlock()
	require.Len(t, saver.calls, 2)
	assert.Equal(t, savedCall{clientID: "c1", tenant: "a"}, saver.calls[0])
	assert.Equal(t, savedCall{clientID: "c2", tenant: "b"}, saver.calls[1])
}

func TestQueueWithoutTenantSavesWithoutTenant(t *testing.T) {
	saver := &recordingSaver{saved: make(chan struct{}, 10)}
	q := monitoring.NewMeasurementQueuing(tenant.WithID(context.Background(), tenant.Default), testLog, saver, 10)
	defer q.Close()

	// a tenant-less enqueue must not silently fall back to the base context's tenant
	require.True(t, q.Notify(context.Background(), models.Measurement{ClientID: "c1"}))
	<-saver.saved

	saver.mu.Lock()
	defer saver.mu.Unlock()
	require.Len(t, saver.calls, 1)
	assert.ErrorIs(t, saver.calls[0].tenantErr, tenant.ErrMissing)
}

func TestQueueDrainsOnCloseWithLiveContext(t *testing.T) {
	saver := &recordingSaver{saved: make(chan struct{}, 10), delay: 20 * time.Millisecond}
	base, cancel := context.WithCancel(context.Background())
	q := monitoring.NewMeasurementQueuing(base, testLog, saver, 10)

	ctx := tenant.WithID(context.Background(), tenant.Default)
	for _, id := range []string{"c1", "c2", "c3"} {
		require.True(t, q.Notify(ctx, models.Measurement{ClientID: id}))
	}
	cancel()
	require.NoError(t, q.Close())

	saver.mu.Lock()
	defer saver.mu.Unlock()
	require.Len(t, saver.calls, 3)
	for _, c := range saver.calls {
		assert.NoError(t, c.ctxErr, c.clientID)
		assert.Equal(t, tenant.Default, c.tenant)
	}
}
