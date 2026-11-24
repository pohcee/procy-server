package schedule

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/share/tenant"
)

func TestCronCallbackInheritsBaseContext(t *testing.T) {
	c := newCron(tenant.WithID(context.Background(), tenant.Default))
	got := make(chan tenant.ID, 1)
	require.NoError(t, c.Add(ctxFor(tenant.Default), "s1", "@every 1s", func(ctx context.Context, _ string) {
		id, _ := tenant.FromContext(ctx)
		select {
		case got <- id:
		default:
		}
	}))
	defer c.Remove(ctxFor(tenant.Default), "s1")
	select {
	case id := <-got:
		assert.Equal(t, tenant.Default, id)
	case <-time.After(3 * time.Second):
		t.Fatal("cron callback did not run")
	}
}

// The schedule manager must refuse to start without a tenant: otherwise every scheduled run that
// targets client groups would fail silently with "no tenant in context".
func TestNewRequiresTenant(t *testing.T) {
	_, err := New(context.Background(), nil, nil, nil, 30)
	assert.ErrorIs(t, err, tenant.ErrMissing)
}

func TestManagerStopStopsSchedules(t *testing.T) {
	m := &Manager{cron: newCron(tenant.WithID(context.Background(), tenant.Default))}
	ran := make(chan struct{}, 10)
	require.NoError(t, m.cron.Add(ctxFor(tenant.Default), "s1", "@every 1s", func(context.Context, string) { ran <- struct{}{} }))

	m.Stop()

	select {
	case <-ran:
		t.Fatal("a schedule ran after Stop")
	case <-time.After(1500 * time.Millisecond):
	}
}
