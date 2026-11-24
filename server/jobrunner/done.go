package jobrunner

import (
	"sync"

	"github.com/openrport/openrport/share/models"
)

// DoneChannels maps the ID of a running sequential multi-client job to the channel its next
// finished job is sent on. It is safe for concurrent use.
type DoneChannels struct {
	m  map[string]chan *models.Job
	mu sync.RWMutex
}

// NewDoneChannels returns an empty map.
func NewDoneChannels() *DoneChannels {
	return &DoneChannels{m: make(map[string]chan *models.Job)}
}

func (m *DoneChannels) Set(jobID string, done chan *models.Job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[jobID] = done
}

func (m *DoneChannels) Del(jobID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.m, jobID)
}

// Get returns the channel of jobID, or nil; a nil map has no channels.
func (m *DoneChannels) Get(jobID string) chan *models.Job {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.m[jobID]
}

// GetAllKeys returns the IDs of the running sequential jobs. Used by tests.
func (m *DoneChannels) GetAllKeys() (jobIDs []string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	jobIDs = make([]string, 0, len(m.m))
	for k := range m.m {
		jobIDs = append(jobIDs, k)
	}
	return jobIDs
}
