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
// It returns false when the entry has none for that OS. For built-in entries
// every {{key}} with a key in vars is replaced in the code; user entries are
// returned verbatim.
func SpecFor(e Entry, osKernel string, vars map[string]string) (JobSpec, bool) {
	key := strings.ToLower(osKernel)
	impl, ok := e.Implementations[key]
	if !ok {
		return JobSpec{}, false
	}
	code := impl.Code
	if e.BuiltinKey != nil {
		code = expandVars(code, vars)
	}
	return JobSpec{
		Command:        code,
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

func expandVars(code string, vars map[string]string) string {
	for k, v := range vars {
		code = strings.ReplaceAll(code, "{{"+k+"}}", v)
	}
	return code
}

// ExpandImplementations returns a copy of impls with every {{key}} of vars
// replaced in the code. Use it for built-in entries only.
func ExpandImplementations(impls map[string]Implementation, vars map[string]string) map[string]Implementation {
	if impls == nil || len(vars) == 0 {
		return impls
	}
	out := make(map[string]Implementation, len(impls))
	for k, impl := range impls {
		impl.Code = expandVars(impl.Code, vars)
		out[k] = impl
	}
	return out
}

// ExpandBuiltin returns e with the {{key}} placeholders of vars filled in its
// implementations when e is a built-in entry; other entries are returned as is.
func ExpandBuiltin(e Entry, vars map[string]string) Entry {
	if e.BuiltinKey != nil {
		e.Implementations = ExpandImplementations(e.Implementations, vars)
	}
	return e
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
	maxTimeout := 0
	for _, impl := range e.Implementations {
		if impl.TimeoutSec > maxTimeout {
			maxTimeout = impl.TimeoutSec
		}
	}
	return maxTimeout
}

// ActiveForRun returns the live entry with its active implementations; 404
// when it is missing or deleted.
func (m *Manager) ActiveForRun(ctx context.Context, id string) (*Entry, error) {
	return m.get(ctx, id, false)
}
