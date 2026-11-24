package jobs

import (
	"context"
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
