package notifications_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/notifications"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/refs"
	"github.com/pohcee/procy-server/share/tenant"
)

type savedState struct {
	id     string
	state  notifications.ProcessingState
	tenant tenant.ID
	err    error
}

// tenantStore records the tenant of the context each notification is saved with.
type tenantStore struct {
	stream chan notifications.NotificationDetails
	saved  chan savedState
}

func (s *tenantStore) record(ctx context.Context, d notifications.NotificationDetails, state notifications.ProcessingState) error {
	id, err := tenant.FromContext(ctx)
	s.saved <- savedState{id: d.ID.ID(), state: state, tenant: id, err: err}
	return err
}

func (s *tenantStore) Create(ctx context.Context, d notifications.NotificationDetails) error {
	id, err := tenant.FromContext(ctx)
	if err != nil {
		return err
	}
	d.Tenant = id
	s.stream <- d
	return nil
}
func (s *tenantStore) SetDone(ctx context.Context, d notifications.NotificationDetails, _ string) error {
	return s.record(ctx, d, notifications.ProcessingStateDone)
}
func (s *tenantStore) SetError(ctx context.Context, d notifications.NotificationDetails, _, _ string) error {
	return s.record(ctx, d, notifications.ProcessingStateError)
}
func (s *tenantStore) NotificationStream(notifications.Target) chan notifications.NotificationDetails {
	return s.stream
}
func (s *tenantStore) Close() error { return nil }

type tenantConsumer struct {
	fail     bool
	tenantCh chan tenant.ID
}

func (c *tenantConsumer) Target() notifications.Target { return notifications.TargetScript }
func (c *tenantConsumer) Process(ctx context.Context, _ notifications.NotificationDetails) (string, error) {
	id, _ := tenant.FromContext(ctx)
	c.tenantCh <- id
	if c.fail {
		return "", errors.New("boom")
	}
	return "", nil
}

func TestProcessorSavesWithTheCreatorsTenant(t *testing.T) {
	for _, fail := range []bool{false, true} {
		store := &tenantStore{stream: make(chan notifications.NotificationDetails, 2), saved: make(chan savedState, 2)}
		consumer := &tenantConsumer{fail: fail, tenantCh: make(chan tenant.ID, 2)}
		p := notifications.NewProcessor(logger.NewLogger("notifications", logger.NewLogOutput(""), logger.LogLevelInfo), store, consumer)

		// created on this goroutine, processed and saved on the processor's
		d := notifications.NewDispatcher(store)
		nid, err := d.Dispatch(tenant.WithID(context.Background(), "a"), refs.GenerateIdentifiable("Problem"), notifications.NotificationData{
			Target: "script.sh", ContentType: notifications.ContentTypeTextPlain,
		})
		require.NoError(t, err)

		select {
		case got := <-store.saved:
			require.NoError(t, got.err)
			assert.Equal(t, nid.ID(), got.id)
			assert.Equal(t, tenant.ID("a"), got.tenant)
		case <-time.After(2 * time.Second):
			t.Fatal("notification was not saved")
		}
		assert.Equal(t, tenant.ID("a"), <-consumer.tenantCh)
		require.NoError(t, p.Close())
	}
}
