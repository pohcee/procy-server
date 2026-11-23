package library

import (
	"context"

	"github.com/openrport/openrport/share/logger"
)

// PruneTask removes old entry versions according to the [library] retention settings.
type PruneTask struct {
	m    *Manager
	keep int
	days int
	log  *logger.Logger
}

func NewPruneTask(m *Manager, keep, days int, log *logger.Logger) *PruneTask {
	return &PruneTask{m: m, keep: keep, days: days, log: log}
}

func (t *PruneTask) Run(ctx context.Context) error {
	n, err := t.m.Prune(ctx, t.keep, t.days)
	if err != nil {
		return err
	}
	if n > 0 && t.log != nil {
		t.log.Infof("library: pruned %d old versions", n)
	}
	return nil
}
