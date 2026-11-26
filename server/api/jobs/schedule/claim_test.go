package schedule

import (
	"context"
	"sync"
	"testing"
	"time"

	cron "github.com/robfig/cron/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/tenant"
)

func mustParse(t *testing.T, spec string) cron.Schedule {
	t.Helper()
	s, err := newCron(context.Background()).cronParser.Parse(spec)
	require.NoError(t, err)
	return s
}

func TestFireTimeSpec(t *testing.T) {
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04:05.000", s, time.Local)
		require.NoError(t, err)
		return v
	}
	everyMinute := mustParse(t, "* * * * *")
	assert.Equal(t, at("2026-11-24 10:05:00.000"), fireTime(everyMinute, at("2026-11-24 10:05:00.003")))
	assert.Equal(t, at("2026-11-24 10:05:00.000"), fireTime(everyMinute, at("2026-11-24 10:05:00.000")), "exactly on time")
	assert.Equal(t, at("2026-11-24 10:05:00.000"), fireTime(everyMinute, at("2026-11-24 10:05:59.900")), "late within the minute")

	daily := mustParse(t, "30 9 * * *")
	assert.Equal(t, at("2026-11-24 09:30:00.000"), fireTime(daily, at("2026-11-24 09:30:00.010")))
}

func TestFireTimeEveryAligned(t *testing.T) {
	c := newCron(tenant.WithID(context.Background(), tenant.Default))
	defer c.Stop()
	c.alignEvery = true
	sch := alignedEvery{every: 30 * time.Second}
	base := time.Unix(1_800_000_000, 0) // a multiple of 30s
	assert.Equal(t, base.Add(30*time.Second), sch.Next(base))
	assert.Equal(t, base.Add(30*time.Second), sch.Next(base.Add(17*time.Second)))
	assert.Equal(t, base.Add(30*time.Second), fireTime(sch, base.Add(30*time.Second+5*time.Millisecond)))
	assert.Equal(t, base, fireTime(sch, base.Add(29*time.Second)))
}

type countingClaimer struct {
	mu     sync.Mutex
	claims map[string]bool
	asked  []time.Time
}

func (c *countingClaimer) ClaimRun(ctx context.Context, id string, fire time.Time) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tid, _ := tenant.FromContext(ctx)
	k := string(tid) + "/" + id + "/" + fire.UTC().Format(time.RFC3339Nano)
	c.asked = append(c.asked, fire)
	if c.claims[k] {
		return false, nil
	}
	c.claims[k] = true
	return true, nil
}

func TestRunStartsOnlyWhenClaimed(t *testing.T) {
	s := &Schedule{
		Base:    Base{ID: "s1", Type: TypeCommand, Schedule: "* * * * *"},
		Details: Details{ClientIDs: []string{"c1"}, Command: "/bin/true", Overlaps: true},
	}
	runner := &fakeRunner{}
	m := NewManager(ctxFor(tenant.Default), runner, &fakeProvider{schedule: s}, logger.NewLogger("t", logger.LogOutput{}, logger.LogLevelError), 30)
	defer m.Stop()
	claimer := &countingClaimer{claims: map[string]bool{}}
	m.SetClaimer(claimer)

	fire := time.Date(2026, 11, 24, 10, 5, 0, 0, time.UTC)
	ctx := withFireTime(ctxFor(tenant.Default), fire)
	m.run(ctx, "s1") // node A
	m.run(ctx, "s1") // node B, same fire time
	assert.Equal(t, 1, runner.typed, "one run per fire time")
	m.run(withFireTime(ctxFor(tenant.Default), fire.Add(time.Minute)), "s1")
	assert.Equal(t, 2, runner.typed, "the next fire time runs again")
	assert.Equal(t, []time.Time{fire, fire, fire.Add(time.Minute)}, claimer.asked)
}

func TestRunWithoutClaimerAlwaysStarts(t *testing.T) {
	s := &Schedule{
		Base:    Base{ID: "s1", Type: TypeCommand, Schedule: "* * * * *"},
		Details: Details{ClientIDs: []string{"c1"}, Command: "/bin/true", Overlaps: true},
	}
	runner := &fakeRunner{}
	m := NewManager(ctxFor(tenant.Default), runner, &fakeProvider{schedule: s}, logger.NewLogger("t", logger.LogOutput{}, logger.LogLevelError), 30)
	defer m.Stop()
	m.run(ctxFor(tenant.Default), "s1")
	m.run(ctxFor(tenant.Default), "s1")
	assert.Equal(t, 2, runner.typed, "standalone: every run starts")
}

func TestCronCallbackCarriesFireTime(t *testing.T) {
	c := newCron(tenant.WithID(context.Background(), tenant.Default))
	defer c.Stop()
	c.alignEvery = true
	got := make(chan time.Time, 1)
	require.NoError(t, c.Add(ctxFor(tenant.Default), "s1", "@every 1s", func(ctx context.Context, _ string) {
		ft, _ := FireTimeFrom(ctx)
		select {
		case got <- ft:
		default:
		}
	}))
	select {
	case ft := <-got:
		assert.Equal(t, ft.Truncate(time.Second), ft, "aligned to the second")
		assert.WithinDuration(t, time.Now(), ft, 2*time.Second)
	case <-time.After(3 * time.Second):
		t.Fatal("cron callback did not run")
	}
}

func TestReloadFollowsStore(t *testing.T) {
	s := &Schedule{Base: Base{ID: "s1", Type: TypeCommand, Schedule: "* * * * *"}}
	p := &fakeProvider{schedule: s}
	m := NewManager(ctxFor(tenant.Default), &fakeRunner{}, p, logger.NewLogger("t", logger.LogOutput{}, logger.LogLevelError), 30)
	defer m.Stop()
	c := m.cron.(*CronImplementation)
	entries := func() int { return len(c.cron.Entries()) }

	require.NoError(t, m.Reload(ctxFor(tenant.Default), "s1"))
	assert.Equal(t, 1, entries(), "created elsewhere: added")
	require.NoError(t, m.Reload(ctxFor(tenant.Default), "s1"))
	assert.Equal(t, 1, entries(), "changed elsewhere: replaced, not added twice")
	p.schedule = nil
	require.NoError(t, m.Reload(ctxFor(tenant.Default), "s1"))
	assert.Equal(t, 0, entries(), "deleted elsewhere: removed")
}
