package schedule

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/server/api/jobs"
	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/server/validation"
	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/random"
	"github.com/openrport/openrport/share/tenant"
)

var (
	supportedSorts = map[string]bool{
		"id":         true,
		"created_at": true,
		"created_by": true,
		"name":       true,
		"type":       true,
	}
	supportedFilters = map[string]bool{
		"id":         true,
		"created_at": true,
		"created_by": true,
		"name":       true,
		"type":       true,
		"client_ids": true,
		"group_ids":  true,
	}
	manualFiltersConfig = map[string]bool{
		"client_ids": true,
		"group_ids":  true,
	}
)

type Provider interface {
	Insert(context.Context, *Schedule) error
	Update(context.Context, *Schedule) error
	List(context.Context, *query.ListOptions) ([]*Schedule, error)
	Get(context.Context, string) (*Schedule, error)
	Delete(context.Context, string) error
	CountJobsInProgress(ctx context.Context, scheduleID string, timeoutSec int) (int, error)
}

type Cron interface {
	Validate(string) error
	Add(ctx context.Context, id, schedule string, f func(context.Context, string)) error
	Remove(ctx context.Context, id string)
}

type JobRunner interface {
	StartMultiClientJob(ctx context.Context, multiJobRequest *jobs.MultiJobRequest) (*models.MultiJob, error)
	StartLibraryMultiJob(ctx context.Context, entryID string, multiJobRequest *jobs.MultiJobRequest) (*models.MultiJob, error)
}

// EntryLookup finds a live library entry, it is satisfied by library.Manager.
type EntryLookup interface {
	ActiveForRun(ctx context.Context, id string) (*library.Entry, error)
}

// MultiJobSaver records a multi-job, used for runs that failed to start.
type MultiJobSaver interface {
	SaveMultiJob(ctx context.Context, multiJob *models.MultiJob) error
}

type Manager struct {
	*logger.Logger
	jobRunner JobRunner
	provider  Provider
	cron      Cron
	entries   EntryLookup
	saver     MultiJobSaver

	runRemoteCmdTimeoutSec int
}

// New returns a manager storing schedules in p, with every stored schedule of ctx's tenant added to cron.
func New(ctx context.Context, logger *logger.Logger, p Provider, jobRunner JobRunner, runRemoteCmdTimeoutSec int) (*Manager, error) {
	// Scheduled runs inherit ctx's tenant; without one every run would fail silently later.
	if _, err := tenant.FromContext(ctx); err != nil {
		return nil, fmt.Errorf("schedule manager: %w", err)
	}

	m := NewManager(ctx, jobRunner, p, logger, runRemoteCmdTimeoutSec)

	existing, err := m.provider.List(ctx, nil)
	if err != nil {
		return nil, err
	}

	for _, cron := range existing {
		err := m.addCron(ctx, cron)
		if err != nil {
			return nil, err
		}
	}

	return m, nil
}

// NewManager returns a manager storing schedules in p, without loading the stored ones into cron.
func NewManager(ctx context.Context, jobRunner JobRunner, p Provider, logger *logger.Logger, runRemoteCmdTimeoutSec int) (m *Manager) {
	m = &Manager{
		Logger:    logger,
		jobRunner: jobRunner,
		provider:  p,
		cron:      newCron(ctx),

		runRemoteCmdTimeoutSec: runRemoteCmdTimeoutSec,
	}
	return m
}

// SetLibrary enables schedules of type library.
func (m *Manager) SetLibrary(entries EntryLookup, saver MultiJobSaver) {
	m.entries = entries
	m.saver = saver
}

// SchedulesUsing lists the library schedules that run the entry.
func (m *Manager) SchedulesUsing(ctx context.Context, entryID string) ([]library.Reference, error) {
	all, err := m.provider.List(ctx, nil)
	if err != nil {
		return nil, err
	}
	refs := []library.Reference{}
	for _, s := range all {
		if s.Type == TypeLibrary && s.Details.LibraryEntryID == entryID {
			refs = append(refs, library.Reference{ID: s.ID, Name: s.Name})
		}
	}
	return refs, nil
}

func (m *Manager) List(ctx context.Context, r *http.Request) (*api.SuccessPayload, error) {
	listOptions := query.GetListOptions(r)

	err := query.ValidateListOptions(listOptions, supportedSorts, supportedFilters, nil /*fields*/, &query.PaginationConfig{
		MaxLimit:     100,
		DefaultLimit: 20,
	})
	if err != nil {
		return nil, err
	}

	manualFilters, dbFilters := query.SplitFilters(listOptions.Filters, manualFiltersConfig)
	pagination := listOptions.Pagination

	listOptions.Filters = dbFilters
	listOptions.Pagination = nil

	entries, err := m.provider.List(ctx, listOptions)
	if err != nil {
		return nil, err
	}

	filtered := make([]*Schedule, 0, len(entries))
	for _, entry := range entries {
		matches, err := query.MatchesFilters(entry, manualFilters)
		if err != nil {
			return nil, err
		}
		if matches {
			filtered = append(filtered, entry)
		}
	}

	totalCount := len(filtered)
	start, end := pagination.GetStartEnd(totalCount)
	limited := filtered[start:end]

	return &api.SuccessPayload{
		Data: limited,
		Meta: api.NewMeta(totalCount),
	}, nil
}

func (m *Manager) Get(ctx context.Context, id string) (*Schedule, error) {
	return m.provider.Get(ctx, id)
}

func (m *Manager) Create(ctx context.Context, s *Schedule, user string) (*Schedule, error) {
	var err error
	s.ID, err = random.UUID4()
	if err != nil {
		return nil, err
	}
	s.CreatedAt = time.Now()
	s.CreatedBy = user

	err = m.validate(s)
	if err == nil {
		err = m.validateEntry(ctx, s)
	}
	if err != nil {
		return nil, err
	}

	err = m.provider.Insert(ctx, s)
	if err != nil {
		return nil, err
	}

	err = m.addCron(ctx, s)
	if err != nil {
		return nil, err
	}

	return s, nil
}

func (m *Manager) Update(ctx context.Context, id string, s *Schedule) (*Schedule, error) {
	s.ID = id

	err := m.validate(s)
	if err == nil {
		err = m.validateEntry(ctx, s)
	}
	if err != nil {
		return nil, err
	}

	err = m.provider.Update(ctx, s)
	if err != nil {
		return nil, err
	}

	m.cron.Remove(ctx, s.ID)
	err = m.addCron(ctx, s)
	if err != nil {
		return nil, err
	}

	s, err = m.provider.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	return s, nil
}

func (m *Manager) Delete(ctx context.Context, id string) error {
	err := m.provider.Delete(ctx, id)
	if err != nil {
		return err
	}

	m.cron.Remove(ctx, id)
	return nil
}

func (m *Manager) validate(s *Schedule) error {
	if s.Type != TypeCommand && s.Type != TypeScript && s.Type != TypeLibrary {
		return &errors.APIError{
			Message:    "Invalid type.",
			Err:        fmt.Errorf("type must be 'command' or 'script'"),
			HTTPStatus: http.StatusBadRequest,
		}
	}

	if s.Type == TypeLibrary {
		if s.Details.LibraryEntryID == "" {
			return &errors.APIError{
				Message:    "Missing library entry.",
				Err:        fmt.Errorf("library_entry_id is required for type library"),
				HTTPStatus: http.StatusBadRequest,
			}
		}
		// the entry defines what runs, store none of the typed fields
		s.Details.Command = ""
		s.Details.Script = ""
		s.Details.Interpreter = ""
		s.Details.Cwd = ""
		s.Details.IsSudo = false
		s.Details.TimeoutSec = 0
	} else if s.Details.LibraryEntryID != "" {
		return &errors.APIError{
			Message:    "Invalid library entry.",
			Err:        fmt.Errorf("library_entry_id is only allowed for type library"),
			HTTPStatus: http.StatusBadRequest,
		}
	}

	err := m.cron.Validate(s.Schedule)
	if err != nil {
		return &errors.APIError{
			Message:    "Invalid schedule.",
			Err:        err,
			HTTPStatus: http.StatusBadRequest,
		}
	}

	err = validation.ValidateInterpreter(s.Details.Interpreter, s.Type == TypeScript)
	if err != nil {
		return &errors.APIError{
			Message:    "Invalid interpreter.",
			Err:        err,
			HTTPStatus: http.StatusBadRequest,
		}
	}

	switch s.Type {
	case TypeCommand:
		if s.Details.Command == "" {
			return &errors.APIError{
				Message:    "Empty command.",
				Err:        fmt.Errorf("command cannot be empty"),
				HTTPStatus: http.StatusBadRequest,
			}
		}
	case TypeScript:
		if s.Details.Script == "" {
			return &errors.APIError{
				Message:    "Empty script.",
				Err:        fmt.Errorf("script cannot be empty"),
				HTTPStatus: http.StatusBadRequest,
			}
		}
		_, err := base64.StdEncoding.DecodeString(s.Details.Script)
		if err != nil {
			return &errors.APIError{
				Message:    "Invalid script.",
				Err:        err,
				HTTPStatus: http.StatusBadRequest,
			}
		}
	}

	return nil
}

// validateEntry checks that a library schedule references a live entry.
func (m *Manager) validateEntry(ctx context.Context, s *Schedule) error {
	if s.Type != TypeLibrary {
		return nil
	}
	if m.entries == nil {
		return fmt.Errorf("library entries are not available")
	}
	if _, err := m.entries.ActiveForRun(ctx, s.Details.LibraryEntryID); err != nil {
		return &errors.APIError{
			Message:    "Library entry not found.",
			Err:        err,
			HTTPStatus: http.StatusBadRequest,
		}
	}
	return nil
}

// addCron schedules s in ctx's tenant, the tenant s belongs to.
func (m *Manager) addCron(ctx context.Context, s *Schedule) error {
	return m.cron.Add(ctx, s.ID, s.Schedule, m.run)
}

func (m *Manager) run(ctx context.Context, id string) {
	schedule, err := m.provider.Get(ctx, id)
	if err != nil {
		m.Errorf("Could not get schedule %s: %v", id, err)
		return
	}
	if schedule == nil {
		// schedule not found in db, probably deleted by user
		return
	}

	var entry *library.Entry
	if schedule.Type == TypeLibrary {
		if m.entries == nil {
			m.recordFailure(ctx, schedule, nil, fmt.Errorf("library entries are not available"))
			return
		}
		entry, err = m.entries.ActiveForRun(ctx, schedule.Details.LibraryEntryID)
		if err != nil {
			m.recordFailure(ctx, schedule, nil, err)
			return
		}
	}

	if !schedule.Details.Overlaps {
		timeoutSec := schedule.Details.TimeoutSec
		if entry != nil {
			timeoutSec = library.MaxTimeout(*entry)
		}
		if timeoutSec <= 0 {
			timeoutSec = m.runRemoteCmdTimeoutSec
		}
		cnt, err := m.provider.CountJobsInProgress(ctx, id, timeoutSec)
		if err != nil {
			m.Errorf("Could not count jobs in progress for schedule %s: %v", id, err)
			return
		}
		if cnt > 0 {
			m.Infof("Skipping non-overlapping schedule %s, because it has jobs in progress.", id)
			return
		}
	}

	m.Infof("Running schedule: %s", id)

	if entry != nil {
		_, err = m.jobRunner.StartLibraryMultiJob(ctx, entry.ID, &jobs.MultiJobRequest{
			ScheduleID:          &schedule.ID,
			Username:            schedule.CreatedBy,
			ClientIDs:           schedule.Details.ClientIDs,
			ClientTags:          schedule.Details.ClientTags,
			GroupIDs:            schedule.Details.GroupIDs,
			ExecuteConcurrently: schedule.Details.ExecuteConcurrently,
			AbortOnError:        schedule.Details.AbortOnError,
		})
		if err != nil {
			m.recordFailure(ctx, schedule, entry, err)
		}
		return
	}

	_, err = m.jobRunner.StartMultiClientJob(ctx, &jobs.MultiJobRequest{
		ScheduleID:          &schedule.ID,
		Username:            schedule.CreatedBy,
		ClientIDs:           schedule.Details.ClientIDs,
		ClientTags:          schedule.Details.ClientTags,
		GroupIDs:            schedule.Details.GroupIDs,
		Command:             schedule.Details.Command,
		Script:              schedule.Details.Script,
		Cwd:                 schedule.Details.Cwd,
		IsSudo:              schedule.Details.IsSudo,
		Interpreter:         schedule.Details.Interpreter,
		TimeoutSec:          schedule.Details.TimeoutSec,
		ExecuteConcurrently: schedule.Details.ExecuteConcurrently,
		AbortOnError:        schedule.Details.AbortOnError,
		IsScript:            schedule.Type == TypeScript,
	})
	if err != nil {
		m.Errorf("Error running schedule %s: %v", id, err)
		return
	}
}

// recordFailure logs a library run that could not start and saves it as a
// multi-job carrying the error, so it shows in the schedule's run history.
func (m *Manager) recordFailure(ctx context.Context, schedule *Schedule, entry *library.Entry, runErr error) {
	m.Errorf("Error running library schedule %s (entry %s): %v", schedule.ID, schedule.Details.LibraryEntryID, runErr)
	if m.saver == nil {
		return
	}
	jid, err := random.UUID4()
	if err != nil {
		m.Errorf("Could not record failed run of schedule %s: %v", schedule.ID, err)
		return
	}
	mj := &models.MultiJob{
		MultiJobSummary: models.MultiJobSummary{
			JID:        jid,
			StartedAt:  time.Now(),
			CreatedBy:  schedule.CreatedBy,
			ScheduleID: &schedule.ID,
		},
		ClientIDs:      schedule.Details.ClientIDs,
		GroupIDs:       schedule.Details.GroupIDs,
		ClientTags:     schedule.Details.ClientTags,
		LibraryEntryID: schedule.Details.LibraryEntryID,
		Error:          runErr.Error(),
	}
	if entry != nil {
		mj.LibraryVersion = entry.ActiveVersion
	}
	if err := m.saver.SaveMultiJob(ctx, mj); err != nil {
		m.Errorf("Could not record failed run of schedule %s: %v", schedule.ID, err)
	}
}
