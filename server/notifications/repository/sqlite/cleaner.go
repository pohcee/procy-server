package sqlite

import (
	"context"
	"time"

	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/random"
)

type Closeable interface {
	Close() error
}

// Cleanable is a notification log the cleaner can delete old entries from.
type Cleanable interface {
	DeleteBefore(ctx context.Context, before time.Time) error
}

type cleaner struct {
	ctx     context.Context
	closer  chan struct{}
	logger  *logger.Logger
	keepFor time.Duration
	repo    Cleanable
}

func (c cleaner) Close() error {
	close(c.closer)
	return nil
}

// StartCleaner periodically deletes the notifications of ctx's tenant that are older than keepFor. It stops on
// Close or when ctx is done.
func StartCleaner(ctx context.Context, logger *logger.Logger, r Cleanable, keepFor time.Duration, checkEvery time.Duration) Closeable {
	c := cleaner{
		ctx:     ctx,
		closer:  make(chan struct{}),
		logger:  logger,
		keepFor: keepFor,
		repo:    r,
	}
	jam := random.AlphaNum(5)
	logger.Infof("started notifications cleaner id: %s", jam)
	go func() {
		c.cleanOld()
		for {
			select {
			case <-time.After(checkEvery):
				logger.Infof("cleaning notifications id: %s", jam)
				c.cleanOld()
			case <-c.closer:
				logger.Infof("closed notifications cleaner id: %s", jam)
				return
			case <-ctx.Done():
				logger.Infof("stopped notifications cleaner id: %s", jam)
				return
			}
		}
	}()

	return c
}

func (c cleaner) cleanOld() {
	before := time.Now().Add(-c.keepFor).UTC()
	c.logger.Infof("cleaning ", before.Format("2006-01-02 15:04:05"))
	err := c.repo.DeleteBefore(c.ctx, before)
	if err != nil {
		c.logger.Errorf("cleaning notifications failed: %v", err)
	}
}

// time.Now().UTC().Add(-time.Second)
