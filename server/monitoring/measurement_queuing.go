package monitoring

import (
	"context"
	"sync"
	"time"

	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/models"
	"github.com/pohcee/procy-server/share/tenant"
)

type saver interface {
	SaveMeasurement(ctx context.Context, measurement *models.Measurement) error
}

type MeasurementSaver interface {
	// Notify enqueues a measurement. It is saved for the tenant carried by ctx.
	Notify(ctx context.Context, measurement models.Measurement) bool
	Close() error
}

// queuedMeasurement carries the tenant captured at enqueue time, so the measurement is saved for the tenant
// of the client it came from.
type queuedMeasurement struct {
	tenant      tenant.ID
	measurement models.Measurement
}

type queue struct {
	saver    saver
	queue    chan queuedMeasurement
	ctx      context.Context
	saveCtx  context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	logger   *logger.Logger
}

func (q *queue) Close() error {
	q.cancelFn() // Signal the context first.
	q.wg.Wait()  // Wait for all the goroutines to finish.
	return nil
}

func (q *queue) Notify(ctx context.Context, measurement models.Measurement) bool {
	// a missing tenant is kept empty: the repository then refuses the save rather than guessing a tenant
	tid, _ := tenant.FromContext(ctx)
	select {
	case <-q.ctx.Done():
		return false
	case q.queue <- queuedMeasurement{tenant: tid, measurement: measurement}:
		return true
	}
}

func (q *queue) process() {
	defer q.wg.Done()

	// Process items until shutdown signal is received
	for {
		select {
		case <-q.ctx.Done():
			q.saveAllEnqueuedMeasurements() // drain chan
			return                          // quit processing
		case m := <-q.queue:
			q.save(m)
		}
	}

}

func (q *queue) save(item queuedMeasurement) {
	ctx := q.saveCtx
	if item.tenant != "" {
		ctx = tenant.WithID(ctx, item.tenant)
	}
	if err := q.saver.SaveMeasurement(ctx, &item.measurement); err != nil {
		q.logger.Errorf("Failed to save measurement for client %s: %s", item.measurement.ClientID, err)
	}
}

func (q *queue) saveAllEnqueuedMeasurements() {
	for {
		select {
		case m := <-q.queue:
			q.save(m)
		default:
			return
		}
	}
}

func (q *queue) logQueueLength() {
	defer q.wg.Done()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-q.ctx.Done():
			return
		case <-ticker.C:
			length := len(q.queue)
			if length > 0 {
				q.logger.Debugf("Enqueued measurements: %v", length)
			}
		}
	}
}

// NewMeasurementQueuing starts a queue that saves measurements in the background. It stops when ctx is done
// or on Close; enqueued measurements are still saved on the way out.
func NewMeasurementQueuing(ctx context.Context, logger *logger.Logger, saver saver, queueSize int) MeasurementSaver {
	// saves outlive the cancellation, so the shutdown drain doesn't fail on a canceled context
	saveCtx := tenant.WithID(context.WithoutCancel(ctx), "")
	ctx, cfn := context.WithCancel(ctx)

	q := &queue{
		saver:    saver,
		queue:    make(chan queuedMeasurement, queueSize),
		ctx:      ctx,
		saveCtx:  saveCtx,
		cancelFn: cfn,
		logger:   logger,
	}

	q.wg.Add(2)
	go q.process()
	go q.logQueueLength()

	return q
}
