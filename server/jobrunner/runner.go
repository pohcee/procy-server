// Package jobrunner runs commands and scripts on clients: single jobs, multi-client jobs (at once or
// one after another) and runs of library entries. The API and the schedules start jobs here; the
// work on each client goes through a dispatch.Dispatcher. Job results come back through the client
// listener, which saves them and signals sequential runs through DoneChannels.
package jobrunner

import (
	"context"
	"errors"

	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/clients/clientdata"
	"github.com/openrport/openrport/server/dispatch"
	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/random"
)

// ErrClientNotConnected is dispatch.ErrClientNotConnected: a sequential run never aborts on it.
var ErrClientNotConnected = dispatch.ErrClientNotConnected

// Targets resolves the clients of a run whose request has none resolved yet (a schedule).
// targeting.Resolver satisfies it.
type Targets interface {
	Ordered(ctx context.Context, clientIDs, groupIDs []string) ([]*clientdata.Client, int, error)
	ByTag(ctx context.Context, tags *models.JobClientTags) ([]*clientdata.Client, error)
}

// JobStore saves the jobs of a run. jobs.Provider satisfies it.
type JobStore interface {
	CreateJob(ctx context.Context, job *models.Job) error
	SaveMultiJob(ctx context.Context, job *models.MultiJob) error
}

// Library gives the entry version a library run uses. library.Manager satisfies it.
type Library interface {
	ActiveForRun(ctx context.Context, id string) (*library.Entry, error)
}

// Options are the runner's dependencies. Jobs, Dispatcher, Targets, Config, Done and Logger are
// required; Library is needed only for library runs.
type Options struct {
	Jobs       JobStore
	Dispatcher dispatch.Dispatcher
	Targets    Targets
	Library    Library
	// Config gives the default command timeout and the public server URL of built-in entries.
	Config *chconfig.Config
	// Done is shared with the client listener, which signals each finished job of a sequential run.
	Done   *DoneChannels
	Logger *logger.Logger

	// NewJobID makes job IDs; nil means random UUIDs.
	NewJobID func() (string, error)
	// OnRunDone, when set, is called each time a multi-client run has sent its last job (and, in a
	// sequential run, waited for its result). Tests wait on it.
	OnRunDone func()
	// NoSequentialWait makes a sequential run send the next job without waiting for the result of the
	// previous one. For tests, where no client answers.
	NoSequentialWait bool
}

// Runner runs jobs on clients.
type Runner struct {
	jobs             JobStore
	dispatcher       dispatch.Dispatcher
	targets          Targets
	library          Library
	config           *chconfig.Config
	done             *DoneChannels
	log              *logger.Logger
	newJobID         func() (string, error)
	onRunDone        func()
	noSequentialWait bool
}

// New returns a runner. It fails when a required option is missing.
func New(o Options) (*Runner, error) {
	switch {
	case o.Jobs == nil:
		return nil, errors.New("jobrunner: Jobs is required")
	case o.Dispatcher == nil:
		return nil, errors.New("jobrunner: Dispatcher is required")
	case o.Targets == nil:
		return nil, errors.New("jobrunner: Targets is required")
	case o.Config == nil:
		return nil, errors.New("jobrunner: Config is required")
	case o.Done == nil:
		return nil, errors.New("jobrunner: Done is required")
	case o.Logger == nil:
		return nil, errors.New("jobrunner: Logger is required")
	}
	newJobID := o.NewJobID
	if newJobID == nil {
		newJobID = random.UUID4
	}
	return &Runner{
		jobs:             o.Jobs,
		dispatcher:       o.Dispatcher,
		targets:          o.Targets,
		library:          o.Library,
		config:           o.Config,
		done:             o.Done,
		log:              o.Logger,
		newJobID:         newJobID,
		onRunDone:        o.OnRunDone,
		noSequentialWait: o.NoSequentialWait,
	}, nil
}

// DoneChannels returns the done channels the runner waits on; the client listener must signal the
// same ones.
func (r *Runner) DoneChannels() *DoneChannels {
	return r.done
}
