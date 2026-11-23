package library

import (
	"context"
	"net/http"

	"github.com/openrport/openrport/server/api/command"
	"github.com/openrport/openrport/server/script"
	"github.com/openrport/openrport/share/types"
)

// This file serves the /library/scripts and /library/commands APIs as views
// over entries: each entry shows its primary implementation.

// Primary returns the implementation shown by the script and command views:
// the first OS in AllOS order that has one.
func Primary(e Entry) (string, Implementation, bool) {
	for _, k := range AllOS {
		if impl, ok := e.Implementations[k]; ok {
			return k, impl, true
		}
	}
	return "", Implementation{}, false
}

func viewTags(e Entry) *types.StringSlice {
	tags := append(types.StringSlice{}, e.Tags...)
	return &tags
}

func ToScript(e Entry) script.Script {
	_, impl, _ := Primary(e)
	created, updated := e.CreatedAt, e.UpdatedAt
	timeout := impl.TimeoutSec
	return script.Script{
		ID: e.ID, Name: e.Name,
		CreatedBy: e.CreatedBy, CreatedAt: &created, UpdatedBy: e.UpdatedBy, UpdatedAt: &updated,
		Interpreter: &impl.Interpreter, IsSudo: &impl.IsSudo, Cwd: &impl.Cwd,
		Script: impl.Code, Tags: viewTags(e), TimoutSec: &timeout,
	}
}

func ToCommand(e Entry) command.Command {
	_, impl, _ := Primary(e)
	created, updated := e.CreatedAt, e.UpdatedAt
	timeout := impl.TimeoutSec
	return command.Command{
		ID: e.ID, Name: e.Name,
		CreatedBy: e.CreatedBy, CreatedAt: &created, UpdatedBy: e.UpdatedBy, UpdatedAt: &updated,
		Cmd: impl.Code, Tags: viewTags(e), TimoutSec: &timeout,
	}
}

// contentWithPrimary returns current's content (empty when nil) with the
// implementation under osKey replaced. When current has an implementation, its
// OS key is used; otherwise defaultOS.
func contentWithPrimary(name string, tags []string, impl Implementation, current *Entry, defaultOS string) Content {
	c := Content{Name: name, Tags: tags, Implementations: map[string]Implementation{}}
	osKey := defaultOS
	if current != nil {
		c.Description = current.Description
		for k, v := range current.Implementations {
			c.Implementations[k] = v
		}
		if k, _, ok := Primary(*current); ok {
			osKey = k
		}
	}
	c.Implementations[osKey] = impl
	return c
}

// ContentFromScript replaces the primary implementation of current with the
// script input, or, for a new entry, creates it under windows when the
// interpreter is cmd or powershell and under linux otherwise.
func ContentFromScript(in script.InputScript, current *Entry) Content {
	osKey := OSLinux
	switch NormalizeInterpreter(in.Interpreter) {
	case "powershell", "cmd":
		osKey = OSWindows
	}
	impl := Implementation{
		Kind: KindScript, Code: in.Script, Interpreter: in.Interpreter, IsSudo: in.IsSudo,
		Cwd: in.Cwd, TimeoutSec: in.TimoutSec,
	}
	return contentWithPrimary(in.Name, in.Tags, impl, current, osKey)
}

// ContentFromCommand is ContentFromScript for commands; a new command goes under linux.
func ContentFromCommand(in command.InputCommand, current *Entry) Content {
	impl := Implementation{Kind: KindCommand, Code: in.Cmd, TimeoutSec: in.TimoutSec}
	return contentWithPrimary(in.Name, in.Tags, impl, current, OSLinux)
}

func isView(e Entry, kind Kind) bool {
	_, impl, ok := Primary(e)
	return ok && impl.EffectiveKind() == kind
}

// ListViews returns every live entry whose primary implementation is of the given kind.
func (m *Manager) ListViews(ctx context.Context, kind Kind) ([]Entry, error) {
	all, err := m.store.ListLive(ctx)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range all {
		if isView(e, kind) {
			out = append(out, e)
		}
	}
	return out, nil
}

// GetView returns a live entry, or a 404 when its primary implementation is not of the given kind.
func (m *Manager) GetView(ctx context.Context, id string, kind Kind) (*Entry, error) {
	e, err := m.Get(ctx, id, false)
	if err != nil {
		return nil, err
	}
	if !isView(*e, kind) {
		return nil, apiErr(http.StatusNotFound, "library entry %q not found", id)
	}
	return e, nil
}

// PruneScript keeps only the requested fields (all when fields is empty).
func PruneScript(s script.Script, fields map[string]bool) script.Script {
	if len(fields) == 0 {
		return s
	}
	out := script.Script{}
	if fields["id"] {
		out.ID = s.ID
	}
	if fields["name"] {
		out.Name = s.Name
	}
	if fields["created_by"] {
		out.CreatedBy = s.CreatedBy
	}
	if fields["created_at"] {
		out.CreatedAt = s.CreatedAt
	}
	if fields["updated_by"] {
		out.UpdatedBy = s.UpdatedBy
	}
	if fields["updated_at"] {
		out.UpdatedAt = s.UpdatedAt
	}
	if fields["interpreter"] {
		out.Interpreter = s.Interpreter
	}
	if fields["is_sudo"] {
		out.IsSudo = s.IsSudo
	}
	if fields["cwd"] {
		out.Cwd = s.Cwd
	}
	if fields["script"] {
		out.Script = s.Script
	}
	if fields["tags"] {
		out.Tags = s.Tags
	}
	if fields["timeout_sec"] {
		out.TimoutSec = s.TimoutSec
	}
	return out
}

// PruneCommand keeps only the requested fields (all when fields is empty).
func PruneCommand(c command.Command, fields map[string]bool) command.Command {
	if len(fields) == 0 {
		return c
	}
	out := command.Command{}
	if fields["id"] {
		out.ID = c.ID
	}
	if fields["name"] {
		out.Name = c.Name
	}
	if fields["created_by"] {
		out.CreatedBy = c.CreatedBy
	}
	if fields["created_at"] {
		out.CreatedAt = c.CreatedAt
	}
	if fields["updated_by"] {
		out.UpdatedBy = c.UpdatedBy
	}
	if fields["updated_at"] {
		out.UpdatedAt = c.UpdatedAt
	}
	if fields["cmd"] {
		out.Cmd = c.Cmd
	}
	if fields["tags"] {
		out.Tags = c.Tags
	}
	if fields["timeout_sec"] {
		out.TimoutSec = c.TimoutSec
	}
	return out
}
