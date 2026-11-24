package jobs

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/mattn/go-sqlite3"
	"github.com/pkg/errors"

	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"
)

const (
	DefaultLimit = 100
	MaxLimit     = 1000
)

var JobSupportedFilters = map[string]bool{
	"jid":                true,
	"started_at[gt]":     true,
	"started_at[lt]":     true,
	"started_at[since]":  true,
	"started_at[until]":  true,
	"finished_at[gt]":    true,
	"finished_at[lt]":    true,
	"finished_at[since]": true,
	"finished_at[until]": true,
	"status":             true,
	"created_by":         true,
	"multi_job_id":       true,
	"schedule_id":        true,
	"client_id":          true,
}
var JobSupportedSorts = map[string]bool{
	"jid":          true,
	"started_at":   true,
	"finished_at":  true,
	"status":       true,
	"multi_job_id": true,
	"schedule_id":  true,
	"created_by":   true,
}
var jobFields = map[string]bool{
	"jid":          true,
	"status":       true,
	"finished_at":  true,
	"client_id":    true,
	"client_name":  true,
	"command":      true,
	"cwd":          true,
	"interpreter":  true,
	"pid":          true,
	"started_at":   true,
	"created_by":   true,
	"timeout_sec":  true,
	"multi_job_id": true,
	"schedule_id":  true,
	"error":        true,
	"is_sudo":      true,
	"is_script":    true,
}
var JobSupportedFields = map[string]map[string]bool{
	"jobs":     jobFields,
	"commands": jobFields,
	"scripts":  jobFields,
	"result": {
		"stdout":  true,
		"stderr":  true,
		"summary": true,
	},
}
var JobListDefaultFields = map[string][]string{
	"fields[jobs]": {
		"jid",
		"status",
		"finished_at",
	},
	"fields[result]": {
		"summary",
	},
}

type SqliteProvider struct {
	log       *logger.Logger
	db        *sqlx.DB
	converter *query.SQLConverter
}

func NewSqliteProvider(db *sqlx.DB, log *logger.Logger) *SqliteProvider {
	return &SqliteProvider{
		db:        db,
		log:       log,
		converter: query.NewSQLConverter(db.DriverName()),
	}
}

// TODO: this was added to support test dependencies. we could potentially remove if there's a
// better way.
func (p *SqliteProvider) GetDB() (db *sqlx.DB) {
	return p.db
}

// tenantOf returns the tenant every query of the provider is scoped to.
func tenantOf(ctx context.Context) (tenant.ID, error) {
	id, err := tenant.FromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("jobs: %w", err)
	}
	return id, nil
}

// jobsQuery selects jobs with the schedule of their multi-job, scoped to one tenant.
const jobsQuery = "SELECT jobs.*, schedule_id FROM jobs LEFT JOIN multi_jobs ON jobs.tenant_id = multi_jobs.tenant_id AND jobs.multi_job_id = multi_jobs.jid WHERE jobs.tenant_id = ?"

func (p *SqliteProvider) GetByJID(ctx context.Context, clientID, jid string) (*models.Job, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	res := &jobSqlite{}
	err = p.db.GetContext(ctx, res, jobsQuery+" AND jobs.jid=?", tid, jid)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return res.convert(), nil
}

func (p *SqliteProvider) List(ctx context.Context, options *query.ListOptions) ([]*models.Job, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	if len(options.Sorts) == 0 {
		options.Sorts = []query.SortOption{
			{
				Column: "finished_at",
				IsASC:  false,
			},
			{
				Column: "jid",
				IsASC:  true,
			},
		}
	}

	q, params := p.converter.AppendOptionsToQuery(options, jobsQuery, []interface{}{tid})

	var res []*jobSqlite
	err = p.db.SelectContext(ctx, &res, q, params...)
	if err != nil {
		return nil, err
	}
	return convertJobs(res), nil
}

func (p *SqliteProvider) Count(ctx context.Context, options *query.ListOptions) (int, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return 0, err
	}
	countOptions := *options
	countOptions.Pagination = nil

	// the tenant filter is on the outer query, so the filters appended below join it with AND
	q := "SELECT count(*) FROM (SELECT jobs.*, schedule_id FROM jobs LEFT JOIN multi_jobs ON jobs.tenant_id = multi_jobs.tenant_id AND jobs.multi_job_id = multi_jobs.jid) WHERE tenant_id = ?"
	q, params := p.converter.AppendOptionsToQuery(&countOptions, q, []interface{}{tid})

	var result int
	err = p.db.GetContext(ctx, &result, q, params...)
	if err != nil {
		return 0, err
	}
	return result, nil
}

// SaveJob creates a new or updates an existing job.
func (p *SqliteProvider) SaveJob(ctx context.Context, job *models.Job) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	_, err = sqlite.WithRetryWhenBusy(func() (result sql.Result, err error) {
		result, err = p.db.NamedExecContext(ctx, `INSERT OR REPLACE INTO jobs (tenant_id, jid, status, started_at, finished_at, created_by, client_id, multi_job_id, details)
		VALUES (:tenant_id, :jid, :status, :started_at, :finished_at, :created_by, :client_id, :multi_job_id, :details)`,
			convertToSqlite(tid, job))
		return result, err
	}, "savejob", p.log)

	if err == nil {
		p.log.Debugf("Job saved successfully: %v", *job)
	}
	return err
}

// CreateJob creates a new job. If already exists with the same ID - does nothing and returns nil.
func (p *SqliteProvider) CreateJob(ctx context.Context, job *models.Job) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	_, err = sqlite.WithRetryWhenBusy(func() (result sql.Result, err error) {
		result, err = p.db.NamedExecContext(ctx, `INSERT INTO jobs (tenant_id, jid, status, started_at, finished_at, created_by, client_id, multi_job_id, details)
		VALUES (:tenant_id, :jid, :status, :started_at, :finished_at, :created_by, :client_id, :multi_job_id, :details)`,
			convertToSqlite(tid, job))
		return result, err
	}, "createjob", p.log)
	if err != nil {
		// check if it's "already exist" err
		typeErr, ok := err.(sqlite3.Error)
		if ok && typeErr.Code == sqlite3.ErrConstraint {
			p.log.Debugf("Job already exist with ID: %s", job.JID)
			if job.LibraryEntryID != "" {
				return p.patchLibraryFields(ctx, tid, job)
			}
			return nil
		}
	} else {
		p.log.Debugf("Job saved successfully: %v", *job)
	}
	return err
}

// patchLibraryFields sets the library fields of an already stored job. A fast
// result can be saved before the job is created, without them.
func (p *SqliteProvider) patchLibraryFields(ctx context.Context, tid tenant.ID, job *models.Job) error {
	_, err := sqlite.WithRetryWhenBusy(func() (sql.Result, error) {
		return p.db.ExecContext(ctx, `UPDATE jobs SET details = json_set(details,
			'$.library_entry_id', ?, '$.library_version', ?, '$.os', ?)
			WHERE tenant_id = ? AND jid = ?`, job.LibraryEntryID, job.LibraryVersion, job.OS, tid, job.JID)
	}, "patchjoblibrary", p.log)
	return err
}

func (p *SqliteProvider) Close() error {
	err := p.db.Close()
	p.log.Debugf("jobs db closed")
	return err
}

type jobSqlite struct {
	TenantID   tenant.ID      `db:"tenant_id"`
	JID        string         `db:"jid"`
	Status     string         `db:"status"`
	FinishedAt sql.NullTime   `db:"finished_at"`
	StartedAt  time.Time      `db:"started_at"`
	CreatedBy  string         `db:"created_by"`
	ClientID   string         `db:"client_id"`
	MultiJobID sql.NullString `db:"multi_job_id"`
	ScheduleID *string        `db:"schedule_id"`
	Details    *JobDetails    `db:"details"`
}

type JobDetails struct {
	Command     string            `json:"command"`
	Cwd         string            `json:"cwd"`
	IsSudo      bool              `json:"is_sudo"`
	IsScript    bool              `json:"is_script"`
	Interpreter string            `json:"interpreter"`
	PID         *int              `json:"pid"`
	TimeoutSec  int               `json:"timeout_sec"`
	Error       string            `json:"error"`
	Result      *models.JobResult `json:"result"`
	ClientName  string            `json:"client_name"`

	LibraryEntryID string `json:"library_entry_id,omitempty"`
	LibraryVersion int    `json:"library_version,omitempty"`
	OS             string `json:"os,omitempty"`
}

func (d *JobDetails) Scan(value interface{}) error {
	if d == nil {
		return errors.New("'details' cannot be nil")
	}
	valueStr, ok := value.(string)
	if !ok {
		return fmt.Errorf("expected to have string, got %T", value)
	}
	err := json.Unmarshal([]byte(valueStr), d)
	if err != nil {
		return fmt.Errorf("failed to decode 'details' field: %v", err)
	}
	return nil
}

func (d *JobDetails) Value() (driver.Value, error) {
	if d == nil {
		return nil, errors.New("'details' cannot be nil")
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("failed to encode 'details' field: %v", err)
	}
	return string(b), nil
}

func convertJobs(list []*jobSqlite) []*models.Job {
	res := make([]*models.Job, 0, len(list))
	for _, cur := range list {
		res = append(res, cur.convert())
	}
	return res
}

func (j *jobSqlite) convert() *models.Job {
	res := &models.Job{
		JID:        j.JID,
		Status:     j.Status,
		ClientID:   j.ClientID,
		StartedAt:  j.StartedAt,
		CreatedBy:  j.CreatedBy,
		ScheduleID: j.ScheduleID,
	}
	if j.Details != nil {
		res.ClientName = j.Details.ClientName
		res.Command = j.Details.Command
		res.Interpreter = j.Details.Interpreter
		res.PID = j.Details.PID
		res.TimeoutSec = j.Details.TimeoutSec
		res.Result = j.Details.Result
		res.Error = j.Details.Error
		res.Cwd = j.Details.Cwd
		res.IsSudo = j.Details.IsSudo
		res.IsScript = j.Details.IsScript
		res.LibraryEntryID = j.Details.LibraryEntryID
		res.LibraryVersion = j.Details.LibraryVersion
		res.OS = j.Details.OS
	}
	if j.FinishedAt.Valid {
		res.FinishedAt = &j.FinishedAt.Time
	}
	if j.MultiJobID.Valid {
		res.MultiJobID = &j.MultiJobID.String
	}
	return res
}

func convertToSqlite(tid tenant.ID, job *models.Job) *jobSqlite {
	res := &jobSqlite{
		TenantID:  tid,
		JID:       job.JID,
		Status:    job.Status,
		StartedAt: job.StartedAt,
		CreatedBy: job.CreatedBy,
		ClientID:  job.ClientID,
		Details: &JobDetails{
			Command:     job.Command,
			Interpreter: job.Interpreter,
			PID:         job.PID,
			TimeoutSec:  job.TimeoutSec,
			Result:      job.Result,
			Error:       job.Error,
			ClientName:  job.ClientName,
			Cwd:         job.Cwd,
			IsSudo:      job.IsSudo,
			IsScript:    job.IsScript,

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
