package schedule

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/query"
)

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
		COUNT(j.jid) FILTER (WHERE j.status != '` + models.JobStatusSkipped + `') AS last_client_count,
		COUNT(j.jid) FILTER (WHERE j.status = '` + models.JobStatusSuccessful + `') AS last_success_count,
		COALESCE(MIN(j.status) FILTER (WHERE j.status != '` + models.JobStatusSkipped + `'), MIN(j.status), CASE WHEN COALESCE(json_extract(mj.details, '$.error'), '') <> '' THEN '` + models.JobStatusFailed + `' END) AS last_status,
		COALESCE(json_extract(mj.details, '$.error'), '') AS last_error,
		COALESCE(MIN(j.details) FILTER (WHERE j.status != '` + models.JobStatusSkipped + `'), MIN(j.details)) AS last_details,
		ROW_NUMBER() OVER (PARTITION BY mj.schedule_id ORDER BY mj.started_at DESC) AS rn
	FROM multi_jobs mj
	LEFT JOIN jobs j ON j.multi_job_id = mj.jid
	GROUP BY mj.jid
	HAVING COUNT(j.jid) > 0 OR COALESCE(json_extract(mj.details, '$.error'), '') <> ''

) mj ON s.id = mj.schedule_id AND mj.rn = 1
`

type SQLiteProvider struct {
	db        *sqlx.DB
	converter *query.SQLConverter
}

func newSQLiteProvider(db *sqlx.DB) *SQLiteProvider {
	return &SQLiteProvider{
		db:        db,
		converter: query.NewSQLConverter(db.DriverName()),
	}
}

func (p *SQLiteProvider) Insert(ctx context.Context, s *Schedule) error {
	_, err := p.db.NamedExecContext(ctx,
		`INSERT INTO schedules (
			id,
			created_at,
			created_by,
			name,
			schedule,
			type,
			details
		) VALUES (
			:id,
			:created_at,
			:created_by,
			:name,
			:schedule,
			:type,
			:details
		)`,
		s.ToDB(),
	)

	return err
}

func (p *SQLiteProvider) Update(ctx context.Context, s *Schedule) error {
	_, err := p.db.NamedExecContext(ctx,
		`UPDATE schedules SET
			name = :name,
			schedule = :schedule,
			type = :type,
			details = :details
		WHERE id = :id`,
		s.ToDB(),
	)

	return err
}

func (p *SQLiteProvider) List(ctx context.Context, options *query.ListOptions) ([]*Schedule, error) {
	values := []*DBSchedule{}

	q, params := p.converter.ConvertListOptionsToQuery(options, schedulesQuery)

	err := p.db.SelectContext(ctx, &values, q, params...)
	if err != nil {
		return nil, err
	}

	result := make([]*Schedule, len(values))
	for i, v := range values {
		result[i] = v.ToSchedule()
	}

	return result, nil
}

func (p *SQLiteProvider) Close() error {
	return p.db.Close()
}

func (p *SQLiteProvider) Get(ctx context.Context, id string) (*Schedule, error) {
	q := schedulesQuery + " WHERE `id` = ? LIMIT 1"

	s := &DBSchedule{}
	err := p.db.GetContext(ctx, s, q, id)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	return s.ToSchedule(), nil
}

func (p *SQLiteProvider) Delete(ctx context.Context, id string) error {
	res, err := p.db.ExecContext(ctx, "DELETE FROM `schedules` WHERE `id` = ?", id)

	if err != nil {
		return err
	}

	affectedRows, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if affectedRows == 0 {
		return fmt.Errorf("cannot find entry by id %s", id)
	}

	// Delete associated jobs
	_, err = p.db.ExecContext(ctx, "DELETE FROM jobs WHERE multi_job_id IN (SELECT jid FROM multi_jobs WHERE schedule_id = ?)", id)
	if err != nil {
		return err
	}

	// Delete associated multi jobs
	_, err = p.db.ExecContext(ctx, "DELETE FROM multi_jobs WHERE schedule_id = ?", id)
	if err != nil {
		return err
	}

	return nil
}

// CountJobsInProgress counts jobs for scheduleID that have not finished and are not timed out
func (p *SQLiteProvider) CountJobsInProgress(ctx context.Context, scheduleID string, timeoutSec int) (int, error) {
	var result int

	err := p.db.GetContext(ctx, &result, `
SELECT count(*)
FROM jobs
JOIN multi_jobs ON jobs.multi_job_id = multi_jobs.jid
WHERE
	schedule_id = ?
AND
	finished_at IS NULL
AND
	strftime('%s', 'now') - strftime('%s', jobs.started_at) <= ?
`, scheduleID, timeoutSec)
	if err != nil {
		return 0, err
	}

	return result, nil
}
