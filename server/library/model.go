// Package library holds the library of reusable commands and scripts: one entry
// has one implementation per client OS and a linear version history.
package library

import (
	"reflect"
	"sort"
	"strings"
	"time"
)

// OS keys are the client's os_kernel values.
const (
	OSLinux   = "linux"
	OSWindows = "windows"
	OSDarwin  = "darwin"
)

// AllOS lists the supported OS keys, in primary-implementation order.
var AllOS = []string{OSLinux, OSWindows, OSDarwin}

const defaultTimeoutSec = 60

type Kind string

const (
	KindAuto    Kind = "auto"
	KindCommand Kind = "command"
	KindScript  Kind = "script"
)

type Implementation struct {
	Kind        Kind   `json:"kind"`
	Code        string `json:"code"`
	Interpreter string `json:"interpreter"`
	IsSudo      bool   `json:"is_sudo"`
	Cwd         string `json:"cwd"`
	TimeoutSec  int    `json:"timeout_sec"`
}

// EffectiveKind resolves KindAuto from the code and interpreter.
func (i Implementation) EffectiveKind() Kind {
	if i.Kind == KindAuto {
		return ResolveKind(i.Code, i.Interpreter)
	}
	return i.Kind
}

// Content is the versioned part of an entry.
type Content struct {
	Name            string                    `json:"name"`
	Description     string                    `json:"description"`
	Tags            []string                  `json:"tags"`
	Implementations map[string]Implementation `json:"implementations"`
}

type Entry struct {
	ID              string                    `json:"id"`
	Name            string                    `json:"name"`
	Description     string                    `json:"description"`
	Tags            []string                  `json:"tags"`
	ActiveVersion   int                       `json:"active_version"`
	BuiltinKey      *string                   `json:"builtin_key,omitempty"`
	OS              []string                  `json:"os"`
	CreatedAt       time.Time                 `json:"created_at"`
	CreatedBy       string                    `json:"created_by"`
	UpdatedAt       time.Time                 `json:"updated_at"`
	UpdatedBy       string                    `json:"updated_by"`
	DeletedAt       *time.Time                `json:"deleted_at,omitempty"`
	DeletedBy       string                    `json:"deleted_by,omitempty"`
	Implementations map[string]Implementation `json:"implementations,omitempty"`
}

type Version struct {
	EntryID   string    `json:"entry_id"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
	Message   string    `json:"message"`
	Changed   []string  `json:"changed"`
	Content   *Content  `json:"content,omitempty"`
}

// Changed lists what differs between two contents: name, description, tags and
// each OS key whose implementation was added, removed or changed. Sorted.
func Changed(old, updated Content) []string {
	changed := []string{}
	if old.Name != updated.Name {
		changed = append(changed, "name")
	}
	if old.Description != updated.Description {
		changed = append(changed, "description")
	}
	if !(len(old.Tags) == 0 && len(updated.Tags) == 0) && !reflect.DeepEqual(old.Tags, updated.Tags) {
		changed = append(changed, "tags")
	}
	keys := map[string]struct{}{}
	for k := range old.Implementations {
		keys[k] = struct{}{}
	}
	for k := range updated.Implementations {
		keys[k] = struct{}{}
	}
	for k := range keys {
		o, oldOK := old.Implementations[k]
		n, newOK := updated.Implementations[k]
		if oldOK != newOK || o != n {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	return changed
}

// Normalize trims the name, dedupes and sorts tags, and fills defaults on each implementation.
func Normalize(c *Content) {
	c.Name = strings.TrimSpace(c.Name)

	seen := map[string]struct{}{}
	tags := []string{}
	for _, t := range c.Tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		tags = append(tags, t)
	}
	sort.Strings(tags)
	c.Tags = tags

	for k, impl := range c.Implementations {
		impl.Interpreter = NormalizeInterpreter(impl.Interpreter)
		if impl.TimeoutSec == 0 {
			impl.TimeoutSec = defaultTimeoutSec
		}
		if impl.Kind == "" {
			impl.Kind = KindAuto
		}
		c.Implementations[k] = impl
	}
}
