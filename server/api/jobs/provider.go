package jobs

import (
	"context"
	"strings"

	"github.com/pohcee/procy-server/share/models"
	"github.com/pohcee/procy-server/share/query"
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
	// a library run: the entry, its version and the OS whose implementation ran
	"library_entry_id": true,
	"library_version":  true,
	"os":               true,
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

// Provider stores jobs and multi-jobs. Every method is scoped to the tenant carried by its context
// and fails with tenant.ErrMissing when there is none.
type Provider interface {
	GetByJID(ctx context.Context, clientID, jid string) (*models.Job, error)
	List(ctx context.Context, options *query.ListOptions) ([]*models.Job, error)
	Count(ctx context.Context, options *query.ListOptions) (int, error)
	// SaveJob creates or updates a job
	SaveJob(ctx context.Context, job *models.Job) error
	// CreateJob creates a new job. If already exist with a given JID - do nothing and return nil
	CreateJob(ctx context.Context, job *models.Job) error
	GetMultiJob(ctx context.Context, jid string) (*models.MultiJob, error)
	GetMultiJobSummaries(ctx context.Context, options *query.ListOptions) ([]*models.MultiJobSummary, error)
	CountMultiJobs(ctx context.Context, options *query.ListOptions) (int, error)
	SaveMultiJob(ctx context.Context, multiJob *models.MultiJob) error
	CleanupJobsMultiJobs(context.Context, int) error
	Close() error
}

// FilterCreatedByOrScheduled names a multi-job filter only the server adds (it is not in MultiJobSupportedFilters, so a
// request cannot send it): it keeps the multi-jobs created by one of its values, and every run of a schedule that
// exists in the tenant. Both providers translate it into SQL themselves, so lists and counts agree.
const FilterCreatedByOrScheduled = "created_by_or_scheduled"

// CreatedByOrScheduled returns the FilterCreatedByOrScheduled filter for username.
func CreatedByOrScheduled(username string) query.FilterOption {
	return query.FilterOption{Column: []string{FilterCreatedByOrScheduled}, Values: []string{username}}
}

// SplitMultiJobFilters takes the FilterCreatedByOrScheduled filters out of filters and returns the rest, with the SQL
// condition (each starting with " AND ", "?" placeholders) and parameters that replace them, on table multi_jobs.
func SplitMultiJobFilters(filters []query.FilterOption) (rest []query.FilterOption, cond string, params []interface{}) {
	for _, f := range filters {
		if len(f.Column) != 1 || f.Column[0] != FilterCreatedByOrScheduled {
			rest = append(rest, f)
			continue
		}
		parts := make([]string, 0, len(f.Values)+1)
		for _, v := range f.Values {
			parts = append(parts, "multi_jobs.created_by = ?")
			params = append(params, v)
		}
		parts = append(parts, "EXISTS (SELECT 1 FROM schedules WHERE schedules.tenant_id = multi_jobs.tenant_id AND schedules.id = multi_jobs.schedule_id)")
		cond += " AND (" + strings.Join(parts, " OR ") + ")"
	}
	return rest, cond, params
}
