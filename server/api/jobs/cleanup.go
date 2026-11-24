package jobs

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
)

type CleanupProvider interface {
	CleanupJobsMultiJobs(context.Context, int) error
}

type CleanupTask struct {
	provider CleanupProvider
	maxJobs  int
}

func NewCleanupTask(provider CleanupProvider, maxJobs int) *CleanupTask {
	return &CleanupTask{
		provider: provider,
		maxJobs:  maxJobs,
	}
}

func (t *CleanupTask) Run(ctx context.Context) error {
	return t.provider.CleanupJobsMultiJobs(ctx, t.maxJobs)
}

// CleanupJobsMultiJobs keeps the newest maxJobs jobs of the context's tenant (with their multi-jobs)
// and deletes the rest of that tenant's jobs and multi-jobs.
func (p *SqliteProvider) CleanupJobsMultiJobs(ctx context.Context, maxJobs int) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	const older = "SELECT %s FROM jobs WHERE tenant_id = ? ORDER BY started_at DESC LIMIT -1 OFFSET ?"
	// Delete all multi jobs that have jobs after max jobs
	_, err = p.db.ExecContext(ctx, "DELETE FROM multi_jobs WHERE tenant_id = ? AND jid IN ("+fmt.Sprintf(older, "multi_job_id")+")", tid, tid, maxJobs)
	if err != nil {
		return errors.Wrap(err, "deleting multi jobs")
	}
	// Delete all jobs associated with multi jobs
	_, err = p.db.ExecContext(ctx, "DELETE FROM jobs WHERE tenant_id = ? AND multi_job_id IN ("+fmt.Sprintf(older, "multi_job_id")+") AND multi_job_id IS NOT NULL", tid, tid, maxJobs)
	if err != nil {
		return errors.Wrap(err, "deleting multi jobs' jobs")
	}
	// Delete any jobs left not from multi jobs
	_, err = p.db.ExecContext(ctx, "DELETE FROM jobs WHERE tenant_id = ? AND jid IN ("+fmt.Sprintf(older, "jid")+") AND multi_job_id IS NULL", tid, tid, maxJobs)
	if err != nil {
		return errors.Wrap(err, "deleting jobs")
	}

	return nil
}
