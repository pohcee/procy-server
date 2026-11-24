package schedule

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
)

// schedulesQuery lists the schedules of one tenant with their last run. Its two parameters are both
// the tenant: the runs are matched inside the subquery (in HAVING, so the query has a single WHERE),
// the schedules by the closing WHERE, so filters appended by the converter join it with AND.
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
	LEFT JOIN jobs j ON j.tenant_id = mj.tenant_id AND j.multi_job_id = mj.jid
	GROUP BY mj.tenant_id, mj.jid
	HAVING mj.tenant_id = ? AND (COUNT(j.jid) > 0 OR COALESCE(json_extract(mj.details, '$.error'), '') <> '')

) mj ON s.id = mj.schedule_id AND mj.rn = 1
WHERE s.tenant_id = ?
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

// tenantOf returns the tenant every query of the provider is scoped to.
func tenantOf(ctx context.Context) (tenant.ID, error) {
	id, err := tenant.FromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("schedules: %w", err)
	}
	return id, nil
}

func (p *SQLiteProvider) Insert(ctx context.Context, s *Schedule) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	_, err = p.db.NamedExecContext(ctx,
		`INSERT INTO schedules (
			tenant_id,
			id,
			created_at,
			created_by,
			name,
			schedule,
			type,
			details
		) VALUES (
			:tenant_id,
			:id,
			:created_at,
			:created_by,
			:name,
			:schedule,
			:type,
			:details
		)`,
		s.ToDB(tid),
	)

	return err
}

func (p *SQLiteProvider) Update(ctx context.Context, s *Schedule) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	_, err = p.db.NamedExecContext(ctx,
		`UPDATE schedules SET
			name = :name,
			schedule = :schedule,
			type = :type,
			details = :details
		WHERE tenant_id = :tenant_id AND id = :id`,
		s.ToDB(tid),
	)

	return err
}

func (p *SQLiteProvider) List(ctx context.Context, options *query.ListOptions) ([]*Schedule, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	values := []*DBSchedule{}

	q, params := p.converter.AppendOptionsToQuery(options, schedulesQuery, []interface{}{tid, tid})

	err = p.db.SelectContext(ctx, &values, q, params...)
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
	tid, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	q := schedulesQuery + " AND s.id = ? LIMIT 1"

	s := &DBSchedule{}
	err = p.db.GetContext(ctx, s, q, tid, tid, id)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	return s.ToSchedule(), nil
}

func (p *SQLiteProvider) Delete(ctx context.Context, id string) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	res, err := p.db.ExecContext(ctx, "DELETE FROM `schedules` WHERE tenant_id = ? AND `id` = ?", tid, id)

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
	_, err = p.db.ExecContext(ctx, "DELETE FROM jobs WHERE tenant_id = ? AND multi_job_id IN (SELECT jid FROM multi_jobs WHERE tenant_id = ? AND schedule_id = ?)", tid, tid, id)
	if err != nil {
		return err
	}

	// Delete associated multi jobs
	_, err = p.db.ExecContext(ctx, "DELETE FROM multi_jobs WHERE tenant_id = ? AND schedule_id = ?", tid, id)
	if err != nil {
		return err
	}

	return nil
}

// CountJobsInProgress counts jobs for scheduleID that have not finished and are not timed out
func (p *SQLiteProvider) CountJobsInProgress(ctx context.Context, scheduleID string, timeoutSec int) (int, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return 0, err
	}
	var result int

	err = p.db.GetContext(ctx, &result, `
SELECT count(*)
FROM jobs
JOIN multi_jobs ON jobs.tenant_id = multi_jobs.tenant_id AND jobs.multi_job_id = multi_jobs.jid
WHERE
	jobs.tenant_id = ?
AND
	schedule_id = ?
AND
	finished_at IS NULL
AND
	strftime('%s', 'now') - strftime('%s', jobs.started_at) <= ?
`, tid, scheduleID, timeoutSec)
	if err != nil {
		return 0, err
	}

	return result, nil
}
