package schedule

import (
	"context"
	"fmt"
	"sync"
	"time"

	cron "github.com/robfig/cron/v3"

	"github.com/openrport/openrport/share/tenant"
)

// cronKey identifies a cron entry: schedule IDs are unique per tenant only.
type cronKey struct {
	tenant tenant.ID
	id     string
}

type CronImplementation struct {
	// ctx is the base context of every callback; each entry runs with its own schedule's tenant on top.
	ctx        context.Context
	mtx        sync.Mutex
	cronParser cron.ScheduleParser
	cron       *cron.Cron
	mapping    map[cronKey]cron.EntryID
	// alignEvery makes "@every d" fire at fixed multiples of d instead of d after the entry was
	// added (multiples counted from Go's zero time), so every node of a cluster fires a schedule at the same instants
	// (and claims the same fire time).
	alignEvery bool
}

func newCron(ctx context.Context) *CronImplementation {
	c := &CronImplementation{
		ctx:        ctx,
		cronParser: cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor),
		cron:       cron.New(cron.WithChain(cron.Recover(cron.DefaultLogger))),
		mapping:    make(map[cronKey]cron.EntryID),
	}
	c.cron.Start()
	return c
}

// Stop stops running schedules; a run in progress continues.
func (c *CronImplementation) Stop() {
	c.cron.Stop()
}

func (c *CronImplementation) Validate(schedule string) error {
	_, err := c.cronParser.Parse(schedule)
	return err
}

// Add schedules f for the schedule id of ctx's tenant. f is called with the cron's base context
// carrying that tenant, so a run outlives the request that created the schedule.
func (c *CronImplementation) Add(ctx context.Context, id string, schedule string, f func(context.Context, string)) error {
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("schedule %s: %w", id, err)
	}

	c.mtx.Lock()
	defer c.mtx.Unlock()

	sch, err := c.cronParser.Parse(schedule)
	if err != nil {
		return err
	}

	if every, ok := sch.(cron.ConstantDelaySchedule); ok && c.alignEvery {
		sch = alignedEvery{every: every.Delay}
	}

	key := cronKey{tenant: tid, id: id}
	if old, ok := c.mapping[key]; ok {
		// never two entries for one schedule
		c.cron.Remove(old)
	}
	runCtx := tenant.WithID(c.ctx, tid)
	entryID := c.cron.Schedule(sch, cron.FuncJob(func() {
		f(withFireTime(runCtx, fireTime(sch, time.Now())), id)
	}))

	c.mapping[key] = entryID

	return nil
}

// Remove removes the entry of the schedule id of ctx's tenant, if any.
func (c *CronImplementation) Remove(ctx context.Context, id string) {
	tid, err := tenant.FromContext(ctx)
	if err != nil {
		return
	}

	c.mtx.Lock()
	defer c.mtx.Unlock()

	key := cronKey{tenant: tid, id: id}
	entryID, ok := c.mapping[key]
	if !ok {
		return
	}
	c.cron.Remove(entryID)
	delete(c.mapping, key)
}

// alignedEvery fires every `every`, at multiples of it counted from Go's zero time (time.Truncate): the same instants
// on every node.
type alignedEvery struct{ every time.Duration }

func (a alignedEvery) Next(t time.Time) time.Time {
	return t.Truncate(a.every).Add(a.every)
}

// fireTime is the time the run of sch starting at now was due: the latest time not after now that sch fires at.
// The cron starts a run a moment after its time, so for a spec (minute resolution) it is the latest match in the
// minute up to now; for an aligned "@every" the multiple of its period now is in.
func fireTime(sch cron.Schedule, now time.Time) time.Time {
	switch s := sch.(type) {
	case alignedEvery:
		return now.Truncate(s.every)
	case cron.ConstantDelaySchedule:
		return now.Truncate(time.Second)
	}
	var fire time.Time
	for t := sch.Next(now.Add(-time.Minute)); !t.IsZero() && !t.After(now); t = sch.Next(t) {
		fire = t
	}
	if fire.IsZero() {
		return now.Truncate(time.Minute)
	}
	return fire
}

type fireTimeKey struct{}

func withFireTime(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, fireTimeKey{}, t)
}

// FireTimeFrom returns the time the scheduled run of ctx was due, if ctx is the context of a run started by the
// cron.
func FireTimeFrom(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(fireTimeKey{}).(time.Time)
	return t, ok
}
