package schedule

import (
	"context"
	"fmt"
	"sync"

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

	runCtx := tenant.WithID(c.ctx, tid)
	entryID := c.cron.Schedule(sch, cron.FuncJob(func() {
		f(runCtx, id)
	}))

	c.mapping[cronKey{tenant: tid, id: id}] = entryID

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
