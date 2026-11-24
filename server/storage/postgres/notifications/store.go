// Package notifications is the Postgres notifications.Repository: the notification log per tenant, and the
// in-process queues the notification processor consumes.
package notifications

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/notifications"
	ossqlite "github.com/openrport/openrport/server/notifications/repository/sqlite"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/refs"
	"github.com/openrport/openrport/share/tenant"

	"github.com/openrport/openrport/server/storage/postgres"
)

// The queue and size limits are the SQLite repository's.
const (
	MaxNotificationsQueue = ossqlite.MaxNotificationsQueue
	MaxOutAndErrorSize    = ossqlite.MaxOutAndErrorSize
	RecipientsSeparator   = ossqlite.RecipientsSeparator
)

// Store keeps the log in the table notifications_log. Create only queues a notification (per target, in this
// process); the processor reads the queue with NotificationStream and logs the outcome with SetDone or SetError.
type Store struct {
	db        *sqlx.DB
	converter *query.SQLConverter
	sinks     map[notifications.Target]chan notifications.NotificationDetails
	closeOnce sync.Once
}

var (
	_ notifications.Repository = (*Store)(nil)
	_ ossqlite.Cleanable       = (*Store)(nil)
)

// New returns the notifications store on db (shared; Close leaves it open).
func New(db *sqlx.DB) *Store {
	sinks := map[notifications.Target]chan notifications.NotificationDetails{}
	for _, target := range notifications.AllTargets {
		sinks[target] = make(chan notifications.NotificationDetails, MaxNotificationsQueue)
	}
	return &Store{db: db, converter: query.NewSQLConverter("postgres"), sinks: sinks}
}

// inTenant runs fn in a transaction of ctx's tenant, with times read and written in UTC (filter values without a
// zone are UTC, as SQLite compares them with its UTC timestamps).
func (s *Store) inTenant(ctx context.Context, fn func(tx *sqlx.Tx, tid tenant.ID) error) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	return postgres.InTenant(ctx, s.db, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, "SET LOCAL TIME ZONE 'UTC'"); err != nil {
			return err
		}
		return fn(tx, tid)
	})
}

// Count counts the log entries matching options (pagination ignored).
func (s *Store) Count(ctx context.Context, options *query.ListOptions) (int, error) {
	var n int
	err := s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		countOptions := query.ListOptions{}
		if options != nil {
			countOptions = *options
		}
		countOptions.Pagination = nil
		countOptions.Sorts = nil
		q, params := s.converter.AppendOptionsToQuery(&countOptions, "SELECT count(*) FROM notifications_log WHERE tenant_id = ?", []interface{}{tid})
		return tx.GetContext(ctx, &n, tx.Rebind(q), params...)
	})
	return n, err
}

// SetError logs that the notification failed.
func (s *Store) SetError(ctx context.Context, details notifications.NotificationDetails, out, err string) error {
	details.Out = out
	details.Err = err
	details.State = notifications.ProcessingStateError
	return s.save(ctx, details)
}

// SetDone logs that the notification was delivered.
func (s *Store) SetDone(ctx context.Context, details notifications.NotificationDetails, out string) error {
	details.State = notifications.ProcessingStateDone
	details.Out = out
	return s.save(ctx, details)
}

// Create queues the notification for its target, stamped with ctx's tenant; it is logged once processed.
func (s *Store) Create(ctx context.Context, details notifications.NotificationDetails) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return err
	}
	// the notification is saved later, on the processor's goroutine: it carries the tenant along
	details.Tenant = tid

	if !details.Target.Valid() {
		return fmt.Errorf("invalid target: %v", details.Target)
	}
	ch := s.sinks[details.Target]
	if len(ch) > MaxNotificationsQueue*0.95 {
		return fmt.Errorf("notification rejected due to too many queued notifications: %v", details.Target)
	}
	ch <- details
	return nil
}

// limit cuts s to at most MaxOutAndErrorSize bytes without splitting a character, after making it storable (see
// storable).
func limit(s string) string {
	s = storable(s)
	if len(s) <= MaxOutAndErrorSize {
		return s
	}
	cut := MaxOutAndErrorSize
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// storable makes s fit a Postgres text column, which (unlike SQLite) rejects NUL bytes and invalid UTF-8: NULs are
// dropped and invalid bytes become U+FFFD.
func storable(s string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "\uFFFD")
}

func (s *Store) save(ctx context.Context, details notifications.NotificationDetails) error {
	return s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO notifications_log (tenant_id, notification_id, content_type, reference_id, transport, recipients, state, subject, body, out, err)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			tid, details.ID.ID(), string(details.Data.ContentType), details.RefID.String(), details.Data.Target,
			strings.Join(details.Data.Recipients, RecipientsSeparator), string(details.State), storable(details.Data.Subject),
			storable(details.Data.Content), limit(details.Out), limit(details.Err))
		return err
	})
}

type logRow struct {
	NotificationID string    `db:"notification_id"`
	Timestamp      time.Time `db:"timestamp"`
	ContentType    string    `db:"content_type"`
	ReferenceID    string    `db:"reference_id"`
	Transport      string    `db:"transport"`
	Recipients     string    `db:"recipients"`
	State          string    `db:"state"`
	Subject        string    `db:"subject"`
	Body           string    `db:"body"`
	Out            string    `db:"out"`
	Err            string    `db:"err"`
}

// Details returns the notification: its data as first logged, its state, out and err as last logged.
func (s *Store) Details(ctx context.Context, nid string) (notifications.NotificationDetails, bool, error) {
	var details notifications.NotificationDetails
	var found bool
	err := s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		var rows []logRow
		if err := tx.SelectContext(ctx, &rows,
			`SELECT notification_id, timestamp, content_type, reference_id, transport, recipients, state, subject, body, out, err
			 FROM notifications_log WHERE tenant_id = $1 AND notification_id = $2 ORDER BY id`, tid, nid); err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		first, last := rows[0], rows[len(rows)-1]
		refID, err := refs.ParseIdentifiable(first.ReferenceID)
		if err != nil {
			return err
		}
		var recipients []string
		if len(first.Recipients) > 0 {
			recipients = strings.Split(first.Recipients, RecipientsSeparator)
		}
		details = notifications.NotificationDetails{
			RefID: refID,
			Data: notifications.NotificationData{
				Target:      first.Transport,
				Recipients:  recipients,
				Subject:     first.Subject,
				Content:     first.Body,
				ContentType: notifications.ContentType(first.ContentType),
			},
			State:  notifications.ProcessingState(last.State),
			ID:     refs.NewIdentifiable(notifications.NotificationType, first.NotificationID),
			Out:    last.Out,
			Err:    last.Err,
			Target: notifications.FigureOutTarget(first.Transport),
			Tenant: tid,
		}
		found = true
		return nil
	})
	if err != nil || !found {
		return notifications.NotificationDetails{}, false, err
	}
	return details, true, nil
}

// List returns the log entries matching options, newest first unless options sort otherwise.
func (s *Store) List(ctx context.Context, options *query.ListOptions) ([]notifications.NotificationSummary, error) {
	var res []notifications.NotificationSummary
	err := s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		listOptions := query.ListOptions{}
		if options != nil {
			listOptions = *options
		}
		if len(listOptions.Sorts) == 0 {
			listOptions.Sorts = []query.SortOption{{Column: "timestamp", IsASC: false}}
		}
		// entries logged at the same time keep the order they were logged in
		listOptions.Sorts = append(append([]query.SortOption{}, listOptions.Sorts...), query.SortOption{Column: "id", IsASC: listOptions.Sorts[0].IsASC})
		q, params := s.converter.AppendOptionsToQuery(&listOptions,
			"SELECT notification_id, state, transport, timestamp, out, err FROM notifications_log WHERE tenant_id = ?", []interface{}{tid})
		var rows []struct {
			State          string    `db:"state"`
			NotificationID string    `db:"notification_id"`
			Transport      string    `db:"transport"`
			Timestamp      time.Time `db:"timestamp"`
			Out            string    `db:"out"`
			Err            string    `db:"err"`
		}
		if err := tx.SelectContext(ctx, &rows, tx.Rebind(q), params...); err != nil {
			return err
		}
		for _, r := range rows {
			res = append(res, notifications.NotificationSummary{
				State:          notifications.ProcessingState(r.State),
				NotificationID: r.NotificationID,
				Transport:      r.Transport,
				// as SQLite's DATETIME column scanned into a string
				Timestamp: r.Timestamp.UTC().Format(time.RFC3339Nano),
				Out:       r.Out,
				Err:       r.Err,
			})
		}
		return nil
	})
	return res, err
}

// NotificationStream returns the queue of the target's notifications.
func (s *Store) NotificationStream(target notifications.Target) chan notifications.NotificationDetails {
	return s.sinks[target]
}

// Close closes the queues (once; later calls do nothing); the database is shared and stays open.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		for _, ch := range s.sinks {
			close(ch)
		}
	})
	return nil
}

// DeleteBefore deletes the notifications of ctx's tenant logged at or before the given time.
func (s *Store) DeleteBefore(ctx context.Context, before time.Time) error {
	return s.inTenant(ctx, func(tx *sqlx.Tx, tid tenant.ID) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM notifications_log WHERE tenant_id = $1 AND timestamp <= $2", tid, before.UTC())
		return err
	})
}
