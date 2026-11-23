package library

import (
	"context"
	"strings"
)

// JobSpec is one entry implementation resolved for a single OS.
type JobSpec struct {
	Command        string
	Interpreter    string
	Cwd            string
	TimeoutSec     int
	IsSudo         bool
	IsScript       bool
	LibraryEntryID string
	LibraryVersion int
	OS             string
}

// SpecFor resolves the entry's implementation for osKernel (case-insensitive).
// It returns false when the entry has none for that OS.
func SpecFor(e Entry, osKernel string) (JobSpec, bool) {
	key := strings.ToLower(osKernel)
	impl, ok := e.Implementations[key]
	if !ok {
		return JobSpec{}, false
	}
	return JobSpec{
		Command:        impl.Code,
		Interpreter:    NormalizeInterpreter(impl.Interpreter),
		Cwd:            impl.Cwd,
		TimeoutSec:     impl.TimeoutSec,
		IsSudo:         impl.IsSudo,
		IsScript:       impl.EffectiveKind() == KindScript,
		LibraryEntryID: e.ID,
		LibraryVersion: e.ActiveVersion,
		OS:             key,
	}, true
}

// OSLabel is the human-readable name of an os_kernel value.
func OSLabel(os string) string {
	switch os {
	case OSLinux:
		return "Linux"
	case OSWindows:
		return "Windows"
	case OSDarwin:
		return "macOS"
	}
	return os
}

// MaxTimeout is the largest timeout_sec among the implementations, 0 when none.
func MaxTimeout(e Entry) int {
	max := 0
	for _, impl := range e.Implementations {
		if impl.TimeoutSec > max {
			max = impl.TimeoutSec
		}
	}
	return max
}

// ActiveForRun returns the live entry with its active implementations; 404
// when it is missing or deleted.
func (m *Manager) ActiveForRun(ctx context.Context, id string) (*Entry, error) {
	return m.get(ctx, id, false)
}
