package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/notifications"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/refs"
	"github.com/openrport/openrport/share/tenant"
)

const MaxNotificationsQueue = 1000
const MaxOutAndErrorSize = 2000 // limit outs to 2k characters

const RecipientsSeparator = "@|@"

// Repository is the SQLite notifications.Repository, kept in notifications.db.
type Repository struct {
	db        *sqlx.DB
	converter *query.SQLConverter
	sinks     map[notifications.Target]chan notifications.NotificationDetails

	L *logger.Logger
}

func tenantOf(ctx context.Context) (tenant.ID, error) {
	id, err := tenant.FromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("notifications: %w", err)
	}
	return id, nil
}

func (r *Repository) Count(ctx context.Context, options *query.ListOptions) (int, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return 0, err
	}
	var res int

	countOptions := *options
	countOptions.Pagination = nil
	q := "SELECT COUNT(*) FROM notifications_log WHERE tenant_id = ?"
	params := []interface{}{tid}
	q, params = r.converter.AppendOptionsToQuery(&countOptions, q, params)

	row := r.db.QueryRowContext(
		ctx,
		q,
		params...,
	)
	err = row.Scan(&res)
	return res, err
}

func (r *Repository) SetError(ctx context.Context, details notifications.NotificationDetails, out, err string) error {
	details.Out = out
	details.Err = err
	details.State = notifications.ProcessingStateError
	return r.save(ctx, details)
}

func (r *Repository) SetDone(ctx context.Context, details notifications.NotificationDetails, out string) error {
	details.State = notifications.ProcessingStateDone
	details.Out = out
	return r.save(ctx, details)
}

type SQLNotification struct {
	TenantID       tenant.ID  `db:"tenant_id"`
	NotificationID string     `db:"notification_id"`
	Timestamp      *time.Time `db:"timestamp"`
	ContentType    string     `db:"contentType"`
	ReferenceID    string     `db:"reference_id"`
	Transport      string     `db:"transport"`
	Recipients     string     `db:"recipients"`
	State          string     `db:"state"`
	Subject        string     `db:"subject"`
	Body           string     `db:"body"`
	Out            string     `db:"out"`
	Err            string     `db:"err"`
}

func (r *Repository) Create(ctx context.Context, details notifications.NotificationDetails) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	// the notification is saved later, on the processor's goroutine: it carries the tenant along
	details.Tenant = tid

	if !details.Target.Valid() {
		return fmt.Errorf("invalid target: %v", details.Target)
	}

	ch := r.sinks[details.Target]
	if len(ch) > MaxNotificationsQueue*0.95 {
		return fmt.Errorf("notification rejected due to too many queued notifications: %v", details.Target)
	}

	ch <- details

	return nil
}

func (r *Repository) save(ctx context.Context, details notifications.NotificationDetails) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}

	n := SQLNotification{
		TenantID:       tid,
		NotificationID: details.ID.ID(),
		Timestamp:      nil,
		ReferenceID:    details.RefID.String(),
		Transport:      details.Data.Target,
		Recipients:     strings.Join(details.Data.Recipients, RecipientsSeparator),
		State:          string(details.State),
		Subject:        details.Data.Subject,
		Body:           details.Data.Content,
		ContentType:    string(details.Data.ContentType),
	}

	if len(details.Out) > MaxNotificationsQueue {
		n.Out = details.Out[:MaxOutAndErrorSize]
	} else {
		n.Out = details.Out
	}

	if len(details.Err) > MaxNotificationsQueue {
		n.Err = details.Err[:MaxOutAndErrorSize]
	} else {
		n.Err = details.Err
	}

	_, err = r.db.NamedExecContext(
		ctx,
		"INSERT INTO `notifications_log` "+
			" (`tenant_id`, `notification_id`, `contentType`, `reference_id`, `transport`, `recipients`, `state`, `subject`, `body`, `out`, `err`)"+
			" VALUES "+
			"(:tenant_id, :notification_id, :contentType, :reference_id, :transport, :recipients, :state, :subject, :body, :out, :err)",
		n,
	)

	return err
}

func (r *Repository) Details(ctx context.Context, nid string) (notifications.NotificationDetails, bool, error) {
	empty := notifications.NotificationDetails{}
	tid, err := tenantOf(ctx)
	if err != nil {
		return empty, false, err
	}
	q := "SELECT * FROM `notifications_log` WHERE `tenant_id` = ? AND `notification_id` = ? order by oid asc"

	entities := []SQLNotification{}
	err = r.db.SelectContext(ctx, &entities, q, tid, nid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return empty, false, nil
		}

		return empty, false, err
	}
	if len(entities) == 0 {
		return empty, false, nil
	}
	entity := entities[0]

	refID, err := refs.ParseIdentifiable(entity.ReferenceID)
	if err != nil {
		return notifications.NotificationDetails{}, false, err
	}

	var recipients []string
	if len(entity.Recipients) > 0 {
		recipients = strings.Split(entity.Recipients, RecipientsSeparator)
	}

	last := entities[len(entities)-1]
	details := notifications.NotificationDetails{
		RefID: refID,
		Data: notifications.NotificationData{
			Target:      entity.Transport,
			Recipients:  recipients,
			Subject:     entity.Subject,
			Content:     entity.Body,
			ContentType: notifications.ContentType(entity.ContentType),
		},
		State:  notifications.ProcessingState(last.State),
		ID:     refs.NewIdentifiable(notifications.NotificationType, entity.NotificationID),
		Out:    last.Out,
		Err:    last.Err,
		Target: notifications.FigureOutTarget(entity.Transport),
		Tenant: entity.TenantID,
	}
	tmp := details

	return tmp, true, nil
}

func (r *Repository) List(ctx context.Context, options *query.ListOptions) ([]notifications.NotificationSummary, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var res []notifications.NotificationSummary

	q := `
SELECT notification_id, state, transport, timestamp, out, err
FROM notifications_log WHERE tenant_id = ?`
	params := []interface{}{tid}
	listOptions := query.ListOptions{}
	if options != nil {
		listOptions = *options
	}
	if len(listOptions.Sorts) == 0 {
		listOptions.Sorts = []query.SortOption{{Column: "timestamp", IsASC: false}}
	}
	q, params = r.converter.AppendOptionsToQuery(&listOptions, q, params)

	err = r.db.SelectContext(
		ctx,
		&res,
		q,
		params...,
	)
	return res, err
}

var (
	_ notifications.Repository = (*Repository)(nil)
	_ Cleanable                = (*Repository)(nil)
)

func NewRepository(connection *sqlx.DB, l *logger.Logger) *Repository {
	sinks := map[notifications.Target]chan notifications.NotificationDetails{}
	for _, target := range notifications.AllTargets {
		sinks[target] = make(chan notifications.NotificationDetails, MaxNotificationsQueue)
	}
	return &Repository{
		db:        connection,
		sinks:     sinks,
		converter: query.NewSQLConverter(connection.DriverName()),
		L:         l,
	}
}

func (r *Repository) NotificationStream(target notifications.Target) chan notifications.NotificationDetails {
	return r.sinks[target]
}

func (r *Repository) Close() error {
	for _, ch := range r.sinks {
		close(ch)
	}
	return nil
}

// DeleteBefore deletes the notifications of ctx's tenant logged at or before the given time.
func (r *Repository) DeleteBefore(ctx context.Context, before time.Time) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx,
		"DELETE FROM `notifications_log` WHERE tenant_id = ? AND timestamp <= ?",
		tid,
		before.UTC().Format("2006-01-02 15:04:05"),
	)
	return err
}
