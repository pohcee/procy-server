// Package jobs stores jobs, multi-client jobs and schedules in Postgres: Store implements the OSS jobs.Provider and
// ScheduleStore the schedule.Provider, on the same tables (a schedule's last run, its jobs in progress and the runs
// deleted with it come from the jobs and multi_jobs tables).
//
// Every method runs in one tenant transaction (postgres.InTenant) and filters tenant_id itself; row-level security is the
// second layer. The details columns hold the JSON the OSS Go types marshal, as text, so rows imported from SQLite
// read back unchanged.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	ossjobs "github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"

	"github.com/openrport/openrport/server/storage/postgres"
)

var _ ossjobs.Provider = (*Store)(nil)

// Store is the jobs.Provider on Postgres.
type Store struct {
	db        *sqlx.DB
	converter *query.SQLConverter
}

// New returns the jobs store on db (migrated; shared, closed by its owner).
func New(db *sqlx.DB) *Store {
	return &Store{db: db, converter: query.NewSQLConverter("postgres")}
}

// Close does nothing: the database is shared and closed by its owner.
func (s *Store) Close() error { return nil }

// in runs fn in a transaction of ctx's tenant, refusing a ctx without one before touching the database.
func in(ctx context.Context, db *sqlx.DB, fn func(tx *sqlx.Tx, tid tenant.ID) error) error {
	tid, err := postgres.TenantOf(ctx)
	if err != nil {
		return fmt.Errorf("jobs: %w", err)
	}
	return postgres.InTenantID(ctx, db, tid, func(tx *sqlx.Tx) error { return fn(tx, tid) })
}

// jobsQuery selects jobs with the schedule of their multi-job; its two parameters are the tenant. The join is in a
// subquery so that filters and sorts on columns both tables have (jid, started_at, created_by) name the job's.
const jobsQuery = `SELECT j.* FROM (
	SELECT jobs.*, multi_jobs.schedule_id FROM jobs
	LEFT JOIN multi_jobs ON jobs.tenant_id = multi_jobs.tenant_id AND jobs.multi_job_id = multi_jobs.jid
	WHERE jobs.tenant_id = ?
) j WHERE j.tenant_id = ?`

// nullable lists the sortable columns that can be NULL, per table.
var (
	jobNullable      = map[string]bool{"finished_at": true, "multi_job_id": true, "schedule_id": true}
	multiJobNullable = map[string]bool{"schedule_id": true}
)

// nullsLikeSQLite returns sorts that order NULL lowest, as SQLite does: first ascending, last descending (Postgres
// does the opposite by default).
func nullsLikeSQLite(sorts []query.SortOption, nullable map[string]bool) []query.SortOption {
	out := make([]query.SortOption, 0, len(sorts))
	for _, s := range sorts {
		if nullable[s.Column] {
			out = append(out, query.SortOption{Column: "(" + s.Column + " IS NULL)", IsASC: !s.IsASC})
		}
		out = append(out, s)
	}
	return out
}

// build appends the list options to q and rebinds it for Postgres.
func (s *Store) build(tx *sqlx.Tx, o *query.ListOptions, q string, params []interface{}) (string, []interface{}) {
	q, params = s.converter.AppendOptionsToQuery(o, q, params)
	return tx.Rebind(q), params
}

// GetByJID returns the job jid of the client clientID, or nil when the client has no such job.
func (s *Store) GetByJID(ctx context.Context, clientID, jid string) (*models.Job, error) {
	var res *models.Job
	err := in(ctx, s.db, func(tx *sqlx.Tx, tid tenant.ID) error {
		row := &jobRow{}
		err := tx.GetContext(ctx, row, tx.Rebind(jobsQuery+" AND j.client_id = ? AND j.jid = ?"), tid, tid, clientID, jid)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		res = row.convert()
		return nil
	})
	return res, err
}

func (s *Store) List(ctx context.Context, options *query.ListOptions) ([]*models.Job, error) {
	var res []*models.Job
	err := in(ctx, s.db, func(tx *sqlx.Tx, tid tenant.ID) error {
		var err error
		res, err = s.listJobs(ctx, tx, tid, options)
		return err
	})
	return res, err
}

func (s *Store) listJobs(ctx context.Context, tx *sqlx.Tx, tid tenant.ID, options *query.ListOptions) ([]*models.Job, error) {
	o := query.ListOptions{}
	if options != nil {
		o = *options
	}
	if len(o.Sorts) == 0 {
		o.Sorts = []query.SortOption{{Column: "finished_at", IsASC: false}, {Column: "jid", IsASC: true}}
	}
	o.Sorts = nullsLikeSQLite(o.Sorts, jobNullable)
	q, params := s.build(tx, &o, jobsQuery, []interface{}{tid, tid})
	var rows []*jobRow
	if err := tx.SelectContext(ctx, &rows, q, params...); err != nil {
		return nil, err
	}
	res := make([]*models.Job, 0, len(rows))
	for _, r := range rows {
		res = append(res, r.convert())
	}
	return res, nil
}

func (s *Store) Count(ctx context.Context, options *query.ListOptions) (int, error) {
	var n int
	err := in(ctx, s.db, func(tx *sqlx.Tx, tid tenant.ID) error {
		o := query.ListOptions{}
		if options != nil {
			o.Filters = options.Filters
		}
		q, params := s.build(tx, &o, strings.Replace(jobsQuery, "SELECT j.*", "SELECT count(*)", 1), []interface{}{tid, tid})
		return tx.GetContext(ctx, &n, q, params...)
	})
	return n, err
}

const insertJob = `INSERT INTO jobs (tenant_id, jid, status, started_at, finished_at, created_by, client_id, multi_job_id, details)
	VALUES (:tenant_id, :jid, :status, :started_at, :finished_at, :created_by, :client_id, :multi_job_id, :details)`

// SaveJob creates a new or replaces an existing job.
func (s *Store) SaveJob(ctx context.Context, job *models.Job) error {
	return in(ctx, s.db, func(tx *sqlx.Tx, tid tenant.ID) error {
		_, err := tx.NamedExecContext(ctx, insertJob+` ON CONFLICT (tenant_id, jid) DO UPDATE SET
			status = EXCLUDED.status, started_at = EXCLUDED.started_at, finished_at = EXCLUDED.finished_at,
			created_by = EXCLUDED.created_by, client_id = EXCLUDED.client_id, multi_job_id = EXCLUDED.multi_job_id,
			details = EXCLUDED.details`, toJobRow(tid, job))
		return err
	})
}

// CreateJob creates a new job. If one with the same JID exists it is kept, except that the library fields of job
// (when set) are written into it: a fast result can be saved before the job is created, without them.
func (s *Store) CreateJob(ctx context.Context, job *models.Job) error {
	return in(ctx, s.db, func(tx *sqlx.Tx, tid tenant.ID) error {
		res, err := tx.NamedExecContext(ctx, insertJob+" ON CONFLICT (tenant_id, jid) DO NOTHING", toJobRow(tid, job))
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n > 0 || job.LibraryEntryID == "" {
			return err
		}
		return patchLibraryFields(ctx, tx, tid, job)
	})
}

// patchLibraryFields is the SQLite json_set of the library fields, done in Go so details stays what Go marshals.
func patchLibraryFields(ctx context.Context, tx *sqlx.Tx, tid tenant.ID, job *models.Job) error {
	var raw string
	err := tx.GetContext(ctx, &raw, "SELECT details FROM jobs WHERE tenant_id = $1 AND jid = $2 FOR UPDATE", tid, job.JID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	d := &ossjobs.JobDetails{}
	if err := d.Scan(raw); err != nil {
		return err
	}
	d.LibraryEntryID, d.LibraryVersion, d.OS = job.LibraryEntryID, job.LibraryVersion, job.OS
	v, err := d.Value()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE jobs SET details = $1 WHERE tenant_id = $2 AND jid = $3", v, tid, job.JID)
	return err
}

// CleanupJobsMultiJobs keeps the newest maxJobs jobs of the context's tenant (with their multi-jobs) and deletes
// the rest of that tenant's jobs and multi-jobs. The steps are the SQLite implementation's, in one transaction.
func (s *Store) CleanupJobsMultiJobs(ctx context.Context, maxJobs int) error {
	return in(ctx, s.db, func(tx *sqlx.Tx, tid tenant.ID) error {
		const older = "SELECT %s FROM jobs WHERE tenant_id = $1 ORDER BY started_at DESC OFFSET $2"
		// multi-jobs that have a job beyond maxJobs (their jobs go next; the reference is checked at commit)
		if _, err := tx.ExecContext(ctx, "DELETE FROM multi_jobs WHERE tenant_id = $1 AND jid IN ("+fmt.Sprintf(older, "multi_job_id")+")", tid, maxJobs); err != nil {
			return fmt.Errorf("deleting multi jobs: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE tenant_id = $1 AND multi_job_id IN ("+fmt.Sprintf(older, "multi_job_id")+") AND multi_job_id IS NOT NULL", tid, maxJobs); err != nil {
			return fmt.Errorf("deleting multi jobs' jobs: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM jobs WHERE tenant_id = $1 AND jid IN ("+fmt.Sprintf(older, "jid")+") AND multi_job_id IS NULL", tid, maxJobs); err != nil {
			return fmt.Errorf("deleting jobs: %w", err)
		}
		return nil
	})
}

// GetMultiJob returns a multi-client job with its clients' jobs (the newest 100).
func (s *Store) GetMultiJob(ctx context.Context, jid string) (*models.MultiJob, error) {
	var res *models.MultiJob
	err := in(ctx, s.db, func(tx *sqlx.Tx, tid tenant.ID) error {
		row := &multiJobRow{}
		err := tx.GetContext(ctx, row, "SELECT tenant_id, jid, started_at, created_by, schedule_id, details FROM multi_jobs WHERE tenant_id = $1 AND jid = $2", tid, jid)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		mj := row.convert()
		mj.Jobs, err = s.listJobs(ctx, tx, tid, &query.ListOptions{
			Filters:    []query.FilterOption{{Column: []string{"multi_job_id"}, Values: []string{jid}}},
			Sorts:      []query.SortOption{{Column: "started_at", IsASC: false}, {Column: "jid", IsASC: true}},
			Pagination: query.NewPagination(ossjobs.DefaultLimit, 0),
		})
		if err != nil {
			return err
		}
		res = mj
		return nil
	})
	return res, err
}

// GetMultiJobSummaries lists multi-client jobs filtered by options, by default sorted by started_at (descending),
// then jid.
func (s *Store) GetMultiJobSummaries(ctx context.Context, options *query.ListOptions) ([]*models.MultiJobSummary, error) {
	var res []*models.MultiJobSummary
	err := in(ctx, s.db, func(tx *sqlx.Tx, tid tenant.ID) error {
		o := query.ListOptions{}
		if options != nil {
			o = *options
		}
		if len(o.Sorts) == 0 {
			o.Sorts = []query.SortOption{{Column: "started_at", IsASC: false}, {Column: "jid", IsASC: true}}
		}
		o.Sorts = nullsLikeSQLite(o.Sorts, multiJobNullable)
		q, params := s.build(tx, &o, "SELECT jid, started_at, created_by, schedule_id FROM multi_jobs WHERE tenant_id = ?", []interface{}{tid})
		var rows []*multiJobSummaryRow
		if err := tx.SelectContext(ctx, &rows, q, params...); err != nil {
			return err
		}
		res = make([]*models.MultiJobSummary, 0, len(rows))
		for _, r := range rows {
			res = append(res, r.convert())
		}
		return nil
	})
	return res, err
}

// CountMultiJobs counts multi-client jobs filtered by options.
func (s *Store) CountMultiJobs(ctx context.Context, options *query.ListOptions) (int, error) {
	var n int
	err := in(ctx, s.db, func(tx *sqlx.Tx, tid tenant.ID) error {
		o := query.ListOptions{}
		if options != nil {
			o.Filters = options.Filters
		}
		q, params := s.build(tx, &o, "SELECT count(*) FROM multi_jobs WHERE tenant_id = ?", []interface{}{tid})
		return tx.GetContext(ctx, &n, q, params...)
	})
	return n, err
}

// SaveMultiJob creates a new or updates an existing multi-client job (without its jobs).
func (s *Store) SaveMultiJob(ctx context.Context, job *models.MultiJob) error {
	return in(ctx, s.db, func(tx *sqlx.Tx, tid tenant.ID) error {
		_, err := tx.NamedExecContext(ctx, `INSERT INTO multi_jobs (tenant_id, jid, started_at, created_by, schedule_id, details, run_error)
			VALUES (:tenant_id, :jid, :started_at, :created_by, :schedule_id, :details, :run_error)
			ON CONFLICT (tenant_id, jid) DO UPDATE SET started_at = EXCLUDED.started_at, created_by = EXCLUDED.created_by,
				schedule_id = EXCLUDED.schedule_id, details = EXCLUDED.details, run_error = EXCLUDED.run_error`, toMultiJobRow(tid, job))
		return err
	})
}

func utc(t time.Time) time.Time { return t.UTC() }

type jobRow struct {
	TenantID   tenant.ID           `db:"tenant_id"`
	JID        string              `db:"jid"`
	Status     string              `db:"status"`
	FinishedAt sql.NullTime        `db:"finished_at"`
	StartedAt  time.Time           `db:"started_at"`
	CreatedBy  string              `db:"created_by"`
	ClientID   string              `db:"client_id"`
	MultiJobID sql.NullString      `db:"multi_job_id"`
	ScheduleID *string             `db:"schedule_id"`
	Details    *ossjobs.JobDetails `db:"details"`
}

func (j *jobRow) convert() *models.Job {
	res := &models.Job{
		JID:        j.JID,
		Status:     j.Status,
		ClientID:   j.ClientID,
		StartedAt:  utc(j.StartedAt),
		CreatedBy:  j.CreatedBy,
		ScheduleID: j.ScheduleID,
	}
	if d := j.Details; d != nil {
		res.ClientName = d.ClientName
		res.Command = d.Command
		res.Interpreter = d.Interpreter
		res.PID = d.PID
		res.TimeoutSec = d.TimeoutSec
		res.Result = d.Result
		res.Error = d.Error
		res.Cwd = d.Cwd
		res.IsSudo = d.IsSudo
		res.IsScript = d.IsScript
		res.LibraryEntryID = d.LibraryEntryID
		res.LibraryVersion = d.LibraryVersion
		res.OS = d.OS
	}
	if j.FinishedAt.Valid {
		t := utc(j.FinishedAt.Time)
		res.FinishedAt = &t
	}
	if j.MultiJobID.Valid {
		id := j.MultiJobID.String
		res.MultiJobID = &id
	}
	return res
}

func toJobRow(tid tenant.ID, job *models.Job) *jobRow {
	res := &jobRow{
		TenantID:  tid,
		JID:       job.JID,
		Status:    job.Status,
		StartedAt: job.StartedAt,
		CreatedBy: job.CreatedBy,
		ClientID:  job.ClientID,
		Details: &ossjobs.JobDetails{
			Command:        job.Command,
			Interpreter:    job.Interpreter,
			PID:            job.PID,
			TimeoutSec:     job.TimeoutSec,
			Result:         job.Result,
			Error:          job.Error,
			ClientName:     job.ClientName,
			Cwd:            job.Cwd,
			IsSudo:         job.IsSudo,
			IsScript:       job.IsScript,
			LibraryEntryID: job.LibraryEntryID,
			LibraryVersion: job.LibraryVersion,
			OS:             job.OS,
		},
	}
	if job.MultiJobID != nil {
		res.MultiJobID = sql.NullString{String: *job.MultiJobID, Valid: true}
	}
	if job.FinishedAt != nil {
		res.FinishedAt = sql.NullTime{Time: *job.FinishedAt, Valid: true}
	}
	return res
}

type multiJobSummaryRow struct {
	JID        string    `db:"jid"`
	StartedAt  time.Time `db:"started_at"`
	CreatedBy  string    `db:"created_by"`
	ScheduleID *string   `db:"schedule_id"`
}

func (r *multiJobSummaryRow) convert() *models.MultiJobSummary {
	return &models.MultiJobSummary{JID: r.JID, StartedAt: utc(r.StartedAt), CreatedBy: r.CreatedBy, ScheduleID: r.ScheduleID}
}

type multiJobRow struct {
	TenantID tenant.ID `db:"tenant_id"`
	multiJobSummaryRow
	Details *multiJobDetails `db:"details"`
}

// multiJobDetails is the details JSON of a multi-job, field for field the OSS SQLite one (same names, order and
// omitempty), so the stored text is the same.
type multiJobDetails struct {
	ClientIDs   []string              `json:"client_ids"`
	GroupIDs    []string              `json:"group_ids"`
	ClientTags  *models.JobClientTags `json:"tags"`
	Command     string                `json:"command"`
	Interpreter string                `json:"interpreter"`
	Cwd         string                `json:"cwd"`
	IsSudo      bool                  `json:"is_sudo"`
	TimeoutSec  int                   `json:"timeout_sec"`
	Concurrent  bool                  `json:"concurrent"`
	AbortOnErr  bool                  `json:"abort_on_err"`

	LibraryEntryID string `json:"library_entry_id,omitempty"`
	LibraryVersion int    `json:"library_version,omitempty"`
	Error          string `json:"error,omitempty"`
}

func (d *multiJobDetails) Scan(value interface{}) error {
	if d == nil {
		return errors.New("'details' cannot be nil")
	}
	var b []byte
	switch v := value.(type) {
	case string:
		b = []byte(v)
	case []byte:
		b = v
	default:
		return fmt.Errorf("expected to have string, got %T", value)
	}
	if err := json.Unmarshal(b, d); err != nil {
		return fmt.Errorf("failed to decode 'details' field: %v", err)
	}
	return nil
}

func (r *multiJobRow) convert() *models.MultiJob {
	d := r.Details
	if d == nil {
		d = &multiJobDetails{}
	}
	return &models.MultiJob{
		MultiJobSummary: *r.multiJobSummaryRow.convert(),
		ClientIDs:       d.ClientIDs,
		GroupIDs:        d.GroupIDs,
		ClientTags:      d.ClientTags,
		Command:         d.Command,
		Cwd:             d.Cwd,
		IsSudo:          d.IsSudo,
		Interpreter:     d.Interpreter,
		TimeoutSec:      d.TimeoutSec,
		Concurrent:      d.Concurrent,
		AbortOnErr:      d.AbortOnErr,
		LibraryEntryID:  d.LibraryEntryID,
		LibraryVersion:  d.LibraryVersion,
		Error:           d.Error,
	}
}

// multiJobInsert is the row SaveMultiJob writes; details is the marshaled multiJobDetails.
type multiJobInsert struct {
	TenantID tenant.ID `db:"tenant_id"`
	multiJobSummaryRow
	Details  string `db:"details"`
	RunError string `db:"run_error"`
}

func toMultiJobRow(tid tenant.ID, job *models.MultiJob) *multiJobInsert {
	b, _ := json.Marshal(&multiJobDetails{
		ClientIDs:      job.ClientIDs,
		GroupIDs:       job.GroupIDs,
		ClientTags:     job.ClientTags,
		Command:        job.Command,
		Interpreter:    job.Interpreter,
		Cwd:            job.Cwd,
		IsSudo:         job.IsSudo,
		TimeoutSec:     job.TimeoutSec,
		Concurrent:     job.Concurrent,
		AbortOnErr:     job.AbortOnErr,
		LibraryEntryID: job.LibraryEntryID,
		LibraryVersion: job.LibraryVersion,
		Error:          job.Error,
	})
	return &multiJobInsert{
		TenantID: tid,
		multiJobSummaryRow: multiJobSummaryRow{
			JID:        job.JID,
			StartedAt:  job.StartedAt,
			CreatedBy:  job.CreatedBy,
			ScheduleID: job.ScheduleID,
		},
		Details:  string(b),
		RunError: strings.ReplaceAll(job.Error, "\x00", ""),
	}
}

// Columns are a job as the jobs table stores it, with the JSON names the ingest function ingest_job_results takes.
type Columns struct {
	JID        string     `json:"jid"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	CreatedBy  string     `json:"created_by"`
	ClientID   string     `json:"client_id"`
	MultiJobID *string    `json:"multi_job_id"`
	// Details is the job document as SaveJob writes it.
	Details string `json:"details"`
}

// EncodeJob returns job as SaveJob would store it (for a device process, which ships job results through
// ingest_job_results instead of writing the table).
func EncodeJob(job *models.Job) (Columns, error) {
	r := toJobRow("", job)
	v, err := r.Details.Value()
	if err != nil {
		return Columns{}, err
	}
	details, ok := v.(string)
	if !ok {
		if b, isBytes := v.([]byte); isBytes {
			details = string(b)
		} else {
			return Columns{}, fmt.Errorf("jobs: unexpected details type %T", v)
		}
	}
	return Columns{JID: job.JID, Status: job.Status, StartedAt: job.StartedAt, FinishedAt: job.FinishedAt,
		CreatedBy: job.CreatedBy, ClientID: job.ClientID, MultiJobID: job.MultiJobID, Details: details}, nil
}
