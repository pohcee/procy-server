package repotest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/notifications"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/refs"
	"github.com/openrport/openrport/share/tenant"
)

// NotificationsCleanable is the method the notifications cleaner needs; the Notifications suite checks it when the
// repository has it.
type NotificationsCleanable interface {
	DeleteBefore(ctx context.Context, before time.Time) error
}

var notificationProblem = refs.GenerateIdentifiable("Problem")

func newNotification() notifications.NotificationDetails {
	return notifications.NotificationDetails{
		RefID: notificationProblem,
		Data: notifications.NotificationData{
			ContentType: notifications.ContentTypeTextHTML,
			Target:      "test-target",
			Recipients:  []string{"r1@example.com", "r2@example.com"},
			Subject:     "test-subject",
			Content:     "test-content",
		},
		State:  notifications.ProcessingStateQueued,
		ID:     refs.GenerateIdentifiable(notifications.NotificationType),
		Target: notifications.TargetScript,
	}
}

func notificationIDs(list []notifications.NotificationSummary) []string {
	ids := []string{}
	for _, n := range list {
		ids = append(ids, n.NotificationID)
	}
	return ids
}

// Notifications runs the contract tests of notifications.Repository. If the repository implements
// NotificationsCleanable, that is checked too.
func Notifications(t *testing.T, newRepo Factory[notifications.Repository]) {
	a, b := Ctx(TenantA), Ctx(TenantB)
	all := &query.ListOptions{}

	t.Run("RequiresTenant", func(t *testing.T) {
		r := newRepo(t)
		ctx := context.Background()
		n := newNotification()

		_, err := r.List(ctx, &query.ListOptions{})
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, err = r.Count(ctx, &query.ListOptions{})
		assert.ErrorIs(t, err, tenant.ErrMissing)
		_, _, err = r.Details(ctx, n.ID.ID())
		assert.ErrorIs(t, err, tenant.ErrMissing)
		assert.ErrorIs(t, r.Create(ctx, n), tenant.ErrMissing)
		assert.ErrorIs(t, r.SetDone(ctx, n, "out"), tenant.ErrMissing)
		assert.ErrorIs(t, r.SetError(ctx, n, "out", "err"), tenant.ErrMissing)
		if c, ok := r.(NotificationsCleanable); ok {
			assert.ErrorIs(t, c.DeleteBefore(ctx, time.Now()), tenant.ErrMissing)
		}
	})

	t.Run("Empty", func(t *testing.T) {
		r := newRepo(t)
		items, err := r.List(a, nil)
		require.NoError(t, err)
		assert.Len(t, items, 0)
		count, err := r.Count(a, all)
		require.NoError(t, err)
		assert.Zero(t, count)
		_, found, err := r.Details(a, "not-found")
		require.NoError(t, err)
		assert.False(t, found)
	})

	t.Run("Done", func(t *testing.T) {
		r := newRepo(t)
		n := newNotification()
		require.NoError(t, r.SetDone(a, n, "out"))

		want := n
		want.State = notifications.ProcessingStateDone
		want.Out = "out"
		want.Tenant = TenantA
		got, found, err := r.Details(a, n.ID.ID())
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, want, got)
	})

	t.Run("Error", func(t *testing.T) {
		r := newRepo(t)
		n := newNotification()
		n.Data.Target = "smtp"
		n.Target = notifications.TargetMail
		n.Data.Recipients = nil
		require.NoError(t, r.SetError(a, n, "test-out", "test-err"))

		want := n
		want.State = notifications.ProcessingStateError
		want.Out = "test-out"
		want.Err = "test-err"
		want.Tenant = TenantA
		got, found, err := r.Details(a, n.ID.ID())
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, want, got)
	})

	t.Run("TenantOnDetailsIsIgnored", func(t *testing.T) {
		r := newRepo(t)
		n := newNotification()
		n.Tenant = TenantB
		require.NoError(t, r.SetDone(a, n, "out"))
		got, found, err := r.Details(a, n.ID.ID())
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, TenantA, got.Tenant)
		_, found, err = r.Details(b, n.ID.ID())
		require.NoError(t, err)
		assert.False(t, found)
	})

	t.Run("LatestStateWins", func(t *testing.T) {
		r := newRepo(t)
		n := newNotification()
		require.NoError(t, r.SetError(a, n, "first", "boom"))
		later := n
		later.Data.Subject = "changed later"
		require.NoError(t, r.SetDone(a, later, "second"))

		got, found, err := r.Details(a, n.ID.ID())
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, notifications.ProcessingStateDone, got.State)
		assert.Equal(t, "second", got.Out)
		assert.Equal(t, "", got.Err)
		assert.Equal(t, "test-subject", got.Data.Subject, "the data is the first entry's")
	})

	t.Run("OutTrimming", func(t *testing.T) {
		r := newRepo(t)
		n := newNotification()
		long := strings.Repeat("t", 10000)
		require.NoError(t, r.SetError(a, n, long, long))
		got, found, err := r.Details(a, n.ID.ID())
		require.NoError(t, err)
		require.True(t, found)
		assert.Len(t, got.Out, 2000)
		assert.Len(t, got.Err, 2000)

		// within the limit nothing is cut
		n2 := newNotification()
		medium := strings.Repeat("m", 1500)
		require.NoError(t, r.SetError(a, n2, medium, medium))
		got, found, err = r.Details(a, n2.ID.ID())
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, medium, got.Out)
		assert.Equal(t, medium, got.Err)
	})

	t.Run("List", func(t *testing.T) {
		r := newRepo(t)
		n1 := newNotification()
		require.NoError(t, r.SetDone(a, n1, "out"))
		time.Sleep(1100 * time.Millisecond) // timestamps may have a resolution of one second
		n2 := newNotification()
		n2.Data.Target = "smtp"
		require.NoError(t, r.SetError(a, n2, "test-out", "test-err"))

		list, err := r.List(a, nil)
		require.NoError(t, err)
		require.Len(t, list, 2)
		// newest first by default
		assert.Equal(t, notifications.NotificationSummary{
			State: notifications.ProcessingStateError, NotificationID: n2.ID.ID(), Transport: "smtp",
			Timestamp: list[0].Timestamp, Out: "test-out", Err: "test-err",
		}, list[0])
		assert.Equal(t, notifications.NotificationSummary{
			State: notifications.ProcessingStateDone, NotificationID: n1.ID.ID(), Transport: "test-target",
			Timestamp: list[1].Timestamp, Out: "out",
		}, list[1])
		for _, s := range list {
			ts, err := time.Parse(time.RFC3339Nano, s.Timestamp)
			require.NoError(t, err, s.Timestamp)
			assert.WithinDuration(t, time.Now(), ts, time.Minute)
		}

		asc, err := r.List(a, &query.ListOptions{Sorts: []query.SortOption{{Column: "timestamp", IsASC: true}}})
		require.NoError(t, err)
		assert.Equal(t, []string{n1.ID.ID(), n2.ID.ID()}, notificationIDs(asc))

		byState, err := r.List(a, &query.ListOptions{Sorts: []query.SortOption{{Column: "state", IsASC: true}}})
		require.NoError(t, err)
		assert.Equal(t, []string{n1.ID.ID(), n2.ID.ID()}, notificationIDs(byState), "done before error")

		count, err := r.Count(a, all)
		require.NoError(t, err)
		assert.Equal(t, 2, count)
	})

	t.Run("ListFiltersAndPagination", func(t *testing.T) {
		r := newRepo(t)
		var ids []string
		for i, state := range []string{"done", "error", "done"} {
			n := newNotification()
			n.RefID = refs.NewIdentifiable("Problem", []string{"p1", "p2", "p3"}[i])
			n.Data.Subject = []string{"Disk full", "CPU high", "Disk slow"}[i]
			if state == "done" {
				require.NoError(t, r.SetDone(a, n, ""))
			} else {
				require.NoError(t, r.SetError(a, n, "", "boom"))
			}
			ids = append(ids, n.ID.ID())
		}
		filter := func(col, val string) *query.ListOptions {
			return &query.ListOptions{
				Filters: []query.FilterOption{{Column: []string{col}, Operator: query.FilterOperatorTypeEQ, Values: []string{val}}},
				Sorts:   []query.SortOption{{Column: "timestamp", IsASC: true}},
			}
		}

		done, err := r.List(a, filter("state", "done"))
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{ids[0], ids[2]}, notificationIDs(done))
		count, err := r.Count(a, filter("state", "done"))
		require.NoError(t, err)
		assert.Equal(t, 2, count)

		byRef, err := r.List(a, filter("reference_id", refs.NewIdentifiable("Problem", "p2").String()))
		require.NoError(t, err)
		assert.Equal(t, []string{ids[1]}, notificationIDs(byRef))

		bySubject, err := r.List(a, filter("subject", "disk*"))
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{ids[0], ids[2]}, notificationIDs(bySubject))

		byTransport, err := r.List(a, filter("transport", "other"))
		require.NoError(t, err)
		assert.Empty(t, byTransport)

		either := &query.ListOptions{Filters: []query.FilterOption{{Column: []string{"state"}, Values: []string{"done", "error"}}}}
		count, err = r.Count(a, either)
		require.NoError(t, err)
		assert.Equal(t, 3, count)

		since := &query.ListOptions{Filters: []query.FilterOption{{Column: []string{"timestamp"}, Operator: query.FilterOperatorTypeGT,
			Values: []string{time.Now().UTC().Add(-time.Hour).Format("2006-01-02 15:04:05")}}}}
		count, err = r.Count(a, since)
		require.NoError(t, err)
		assert.Equal(t, 3, count)
		future := &query.ListOptions{Filters: []query.FilterOption{{Column: []string{"timestamp"}, Operator: query.FilterOperatorTypeGT,
			Values: []string{time.Now().UTC().Add(time.Hour).Format("2006-01-02 15:04:05")}}}}
		count, err = r.Count(a, future)
		require.NoError(t, err)
		assert.Zero(t, count)

		page := &query.ListOptions{Pagination: &query.Pagination{Limit: "2", Offset: "1"}}
		paged, err := r.List(a, page)
		require.NoError(t, err)
		assert.Len(t, paged, 2)
		count, err = r.Count(a, page)
		require.NoError(t, err)
		assert.Equal(t, 3, count, "count ignores pagination")
	})

	t.Run("Stream", func(t *testing.T) {
		r := newRepo(t)
		n := newNotification()
		n.Tenant = TenantB // the context's tenant wins
		require.NoError(t, r.Create(a, n))

		select {
		case got := <-r.NotificationStream(notifications.TargetScript):
			want := n
			want.Tenant = TenantA
			assert.Equal(t, want, got)
		case <-time.After(5 * time.Second):
			t.Fatal("the notification was not streamed")
		}

		mail := newNotification()
		mail.Target = notifications.TargetMail
		require.NoError(t, r.Create(b, mail))
		select {
		case got := <-r.NotificationStream(notifications.TargetMail):
			assert.Equal(t, mail.ID, got.ID)
			assert.Equal(t, TenantB, got.Tenant)
		case <-time.After(5 * time.Second):
			t.Fatal("the notification was not streamed")
		}

		// queued notifications are not in the log yet
		_, found, err := r.Details(a, n.ID.ID())
		require.NoError(t, err)
		assert.False(t, found)

		bad := newNotification()
		bad.Target = "pigeon"
		assert.Error(t, r.Create(a, bad))
	})

	t.Run("RejectsWhenQueueIsNearlyFull", func(t *testing.T) {
		r := newRepo(t)
		for i := 0; i < 1000; i++ {
			err := r.Create(a, newNotification())
			if err != nil {
				assert.Greater(t, i, 500)
				assert.ErrorContains(t, err, "rejected")
				return
			}
		}
		t.Fatal("should reject when too many notifications wait")
	})

	t.Run("IsolatesTenants", func(t *testing.T) {
		r := newRepo(t)
		n := newNotification()

		// the same notification ID can exist in both tenants
		require.NoError(t, r.SetDone(a, n, "from a"))
		require.NoError(t, r.SetError(b, n, "from b", "boom"))
		require.NoError(t, r.SetDone(b, newNotification(), ""))

		got, found, err := r.Details(a, n.ID.ID())
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "from a", got.Out)
		assert.Equal(t, notifications.ProcessingStateDone, got.State)
		assert.Equal(t, TenantA, got.Tenant)

		got, found, err = r.Details(b, n.ID.ID())
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "from b", got.Out)
		assert.Equal(t, notifications.ProcessingStateError, got.State)
		assert.Equal(t, TenantB, got.Tenant)

		listA, err := r.List(a, &query.ListOptions{})
		require.NoError(t, err)
		assert.Len(t, listA, 1)
		listB, err := r.List(b, &query.ListOptions{})
		require.NoError(t, err)
		assert.Len(t, listB, 2)

		countA, err := r.Count(a, all)
		require.NoError(t, err)
		assert.Equal(t, 1, countA)
		countB, err := r.Count(b, all)
		require.NoError(t, err)
		assert.Equal(t, 2, countB)

		_, found, err = r.Details(Ctx("c"), n.ID.ID())
		require.NoError(t, err)
		assert.False(t, found)
	})

	t.Run("DeleteBefore", func(t *testing.T) {
		r := newRepo(t)
		c, ok := r.(NotificationsCleanable)
		if !ok {
			t.Skip("the repository does not delete old entries")
		}
		require.NoError(t, r.SetDone(a, newNotification(), ""))
		require.NoError(t, r.SetDone(b, newNotification(), ""))

		require.NoError(t, c.DeleteBefore(a, time.Now().Add(-time.Hour)))
		count, err := r.Count(a, all)
		require.NoError(t, err)
		assert.Equal(t, 1, count, "fresh entries stay")

		require.NoError(t, c.DeleteBefore(a, time.Now().Add(time.Hour)))
		count, err = r.Count(a, all)
		require.NoError(t, err)
		assert.Zero(t, count)
		count, err = r.Count(b, all)
		require.NoError(t, err)
		assert.Equal(t, 1, count, "other tenants are left alone")
	})
}
