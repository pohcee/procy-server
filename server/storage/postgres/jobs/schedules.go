package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/api/jobs/schedule"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
)

var _ schedule.Provider = (*ScheduleStore)(nil)

// ScheduleStore is the schedule.Provider on Postgres, on the tables of Store.
type ScheduleStore struct {
	db        *sqlx.DB
	converter *query.SQLConverter
}

// NewScheduleStore returns the schedule store on db (migrated; shared, closed by its owner).
func NewScheduleStore(db *sqlx.DB) *ScheduleStore {
	return &ScheduleStore{db: db, converter: query.NewSQLConverter("postgres")}
}

// schedulesQuery lists the schedules of one tenant with their last run (the newest multi-job that has jobs or
// an error); both parameters are the tenant. It is the SQLite query with json_extract(details, '$.error') read
// from the run_error column (a cast of details to json(b) fails on a command with a NUL), and MIN over text in byte order (SQLite's) so the same job's details and status are picked.
const schedulesQuery = `
SELECT
	s.*,
	mj.last_started_at,
	mj.last_client_count,
	mj.last_success_count,
	mj.last_status,
	mj.last_details,
	mj.last_error
FROM schedules s
LEFT JOIN (
	SELECT
		mj.schedule_id,
		mj.started_at AS last_started_at,
		COUNT(j.jid) FILTER (WHERE j.status <> '` + models.JobStatusSkipped + `') AS last_client_count,
		COUNT(j.jid) FILTER (WHERE j.status = '` + models.JobStatusSuccessful + `') AS last_success_count,
		COALESCE(
			MIN(j.status COLLATE "C") FILTER (WHERE j.status <> '` + models.JobStatusSkipped + `'),
			MIN(j.status COLLATE "C"),
			CASE WHEN mj.run_error <> '' THEN '` + models.JobStatusFailed + `' END
		) AS last_status,
		mj.run_error AS last_error,
		COALESCE(MIN(j.details COLLATE "C") FILTER (WHERE j.status <> '` + models.JobStatusSkipped + `'), MIN(j.details COLLATE "C")) AS last_details,
		ROW_NUMBER() OVER (PARTITION BY mj.schedule_id ORDER BY mj.started_at DESC) AS rn
	FROM multi_jobs mj
	LEFT JOIN jobs j ON j.tenant_id = mj.tenant_id AND j.multi_job_id = mj.jid
	WHERE mj.tenant_id = ? AND mj.schedule_id IS NOT NULL
	GROUP BY mj.tenant_id, mj.jid
	HAVING COUNT(j.jid) > 0 OR mj.run_error <> ''
) mj ON s.id = mj.schedule_id AND mj.rn = 1 WHERE s.tenant_id = ?`

func (p *ScheduleStore) Insert(ctx context.Context, s *schedule.Schedule) error {
	return in(ctx, p.db, func(tx *sqlx.Tx, tid tenant.ID) error {
		_, err := tx.NamedExecContext(ctx, `INSERT INTO schedules (tenant_id, id, created_at, created_by, name, schedule, type, details)
			VALUES (:tenant_id, :id, :created_at, :created_by, :name, :schedule, :type, :details)`, s.ToDB(tid))
		return err
	})
}

// Update changes the name, schedule, type and details of the schedule (none if there is no such schedule).
func (p *ScheduleStore) Update(ctx context.Context, s *schedule.Schedule) error {
	return in(ctx, p.db, func(tx *sqlx.Tx, tid tenant.ID) error {
		_, err := tx.NamedExecContext(ctx, `UPDATE schedules SET name = :name, schedule = :schedule, type = :type, details = :details
			WHERE tenant_id = :tenant_id AND id = :id`, s.ToDB(tid))
		return err
	})
}

// List lists the schedules with their last run, by default in the order they were created.
func (p *ScheduleStore) List(ctx context.Context, options *query.ListOptions) ([]*schedule.Schedule, error) {
	var res []*schedule.Schedule
	err := in(ctx, p.db, func(tx *sqlx.Tx, tid tenant.ID) error {
		o := query.ListOptions{}
		if options != nil {
			o = *options
		}
		if len(o.Sorts) == 0 {
			o.Sorts = []query.SortOption{{Column: "s.created_at", IsASC: true}, {Column: "s.id", IsASC: true}}
		}
		q, params := p.converter.AppendOptionsToQuery(&o, schedulesQuery, []interface{}{tid, tid})
		var rows []*schedule.DBSchedule
		if err := tx.SelectContext(ctx, &rows, tx.Rebind(q), params...); err != nil {
			return err
		}
		res = make([]*schedule.Schedule, len(rows))
		for i, r := range rows {
			res[i] = toSchedule(r)
		}
		return nil
	})
	return res, err
}

func (p *ScheduleStore) Get(ctx context.Context, id string) (*schedule.Schedule, error) {
	var res *schedule.Schedule
	err := in(ctx, p.db, func(tx *sqlx.Tx, tid tenant.ID) error {
		row := &schedule.DBSchedule{}
		err := tx.GetContext(ctx, row, tx.Rebind(schedulesQuery+" AND s.id = ? LIMIT 1"), tid, tid, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		res = toSchedule(row)
		return nil
	})
	return res, err
}

// Delete deletes the schedule with its runs (multi-jobs and their jobs).
func (p *ScheduleStore) Delete(ctx context.Context, id string) error {
	return in(ctx, p.db, func(tx *sqlx.Tx, tid tenant.ID) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM schedules WHERE tenant_id = $1 AND id = $2", tid, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("cannot find entry by id %s", id)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE tenant_id = $1 AND multi_job_id IN (SELECT jid FROM multi_jobs WHERE tenant_id = $1 AND schedule_id = $2)", tid, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM multi_jobs WHERE tenant_id = $1 AND schedule_id = $2", tid, id)
		return err
	})
}

// CountJobsInProgress counts the jobs of the schedule's runs that have not finished and started at most
// timeoutSec seconds ago (in whole seconds, as SQLite's strftime('%s') compares).
func (p *ScheduleStore) CountJobsInProgress(ctx context.Context, scheduleID string, timeoutSec int) (int, error) {
	var n int
	err := in(ctx, p.db, func(tx *sqlx.Tx, tid tenant.ID) error {
		return tx.GetContext(ctx, &n, `
SELECT count(*)
FROM jobs
JOIN multi_jobs ON jobs.tenant_id = multi_jobs.tenant_id AND jobs.multi_job_id = multi_jobs.jid
WHERE jobs.tenant_id = $1
	AND multi_jobs.schedule_id = $2
	AND jobs.finished_at IS NULL
	AND floor(extract(epoch FROM now())) - floor(extract(epoch FROM jobs.started_at)) <= $3`, tid, scheduleID, timeoutSec)
	})
	return n, err
}

func toSchedule(r *schedule.DBSchedule) *schedule.Schedule {
	r.CreatedAt = utc(r.CreatedAt)
	if r.Execution.StartedAt != nil {
		t := utc(*r.Execution.StartedAt)
		r.Execution.StartedAt = &t
	}
	return r.ToSchedule()
}
